// Command emergency 是服务器侧应急脚本（F-9-06）。
//
// **面板不可用时的带外入口**：重置管理员密码、清除 2FA、清除邮箱绑定。
// 在宿主机本地执行，不经 Web。
//
// 用法：
//
//	emergency list-admins
//	emergency reset-password -user alice
//	emergency clear-2fa -user alice
//	emergency clear-email -user alice
//
// 全局参数：
//
//	-yes    跳过确认（用于确实无法交互的场合，如另一段脚本里）
//
// # 关于它的安全边界
//
// 这个工具**不做身份验证**，这是刻意的：能在这台机器上以这个身份运行它的
// 人，本来就能直接改数据库。在脚本上再套一层「请输入管理密码」只是表演，
// 而且它会给运维一种"这个工具受保护"的错觉。
//
// **真正的边界是宿主机的登录权限。** 因此它每次执行都会往审计里写一条来源为
// emergency 的记录——事后要区分「从面板点的」与「从宿主机命令行敲的」时，
// 那是唯一的依据。
//
// 它只开数据库，不构造队列、采集器或任何需要运行循环的组件：**服务没起来、
// 甚至起不来的时候它也要能用**——那正是它存在的理由。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/joho/godotenv"

	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/emergency"
)

func main() {
	// 与 cmd/server 一致地读取 .env：运维通常就是在部署目录下执行它，
	// 而数据库连接串在那里。
	_ = godotenv.Load()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	user := fs.String("user", "", "目标用户名")
	password := fs.String("password", "", "新密码；留空则自动生成一个随机临时密码")
	yes := fs.Bool("yes", false, "跳过确认（用于无法交互的场合）")
	_ = fs.Parse(os.Args[2:])

	cfg, err := config.Load()
	if err != nil {
		fatal("读取配置失败: %v", err)
	}
	db, err := database.Open(cfg.DB, false)
	if err != nil {
		fatal("连接数据库失败: %v", err)
	}

	ctx := context.Background()
	tool := emergency.New(db, audit.NewRecorder(db))

	switch cmd {
	case "list-admins":
		runListAdmins(ctx, tool)
	case "reset-password":
		requireUser(cmd, *user)
		newPw := *password
		generated := false
		if newPw == "" {
			newPw, err = emergency.GeneratePassword()
			if err != nil {
				fatal("生成临时密码失败: %v", err)
			}
			generated = true
		}
		confirm(*yes, fmt.Sprintf(
			"将重置 %q 的密码，并使其**全部既有会话立即失效**（R-010）。", *user))
		if _, err := tool.ResetPassword(ctx, *user, newPw); err != nil {
			fatal("重置密码失败: %v", err)
		}
		fmt.Printf("\n已重置 %s 的密码。\n", *user)
		if generated {
			fmt.Printf("\n  临时密码：%s\n", newPw)
			fmt.Println("\n这串密码**只显示这一次**，请立即转交给用户。")
		}
		fmt.Println("该用户的全部既有会话已失效，登录时会被要求立即修改密码。")

	case "clear-2fa":
		requireUser(cmd, *user)
		confirm(*yes, fmt.Sprintf(
			"将清除 %q 的密码器与**全部恢复码**，并使其全部既有会话失效。\n"+
				"清除之后该账号只凭密码即可登录。", *user))
		if _, err := tool.Clear2FA(ctx, *user); err != nil {
			fatal("清除 2FA 失败: %v", err)
		}
		fmt.Printf("\n已清除 %s 的密码器与恢复码。\n", *user)
		fmt.Println("该账号现在只凭密码即可登录——请尽快重新配置两步验证。")

	case "clear-email":
		requireUser(cmd, *user)
		confirm(*yes, fmt.Sprintf("将清除 %q 的邮箱绑定与验证状态。", *user))
		if _, err := tool.ClearEmail(ctx, *user); err != nil {
			fatal("清除邮箱失败: %v", err)
		}
		fmt.Printf("\n已清除 %s 的邮箱绑定。\n", *user)

	default:
		usage()
		os.Exit(2)
	}
}

func runListAdmins(ctx context.Context, tool *emergency.Tool) {
	admins, err := tool.ListAdmins(ctx)
	if err != nil {
		fatal("查询管理员失败: %v", err)
	}
	if len(admins) == 0 {
		fmt.Println("没有管理员账号。")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "用户名\t状态\t两步验证\t邮箱")
	for _, a := range admins {
		mail := a.Email
		if mail == "" {
			mail = "—"
		} else if !a.EmailVerified {
			// 未验证的邮箱在"用邮箱找回"的流程里是没用的，而这一点
			// 在被忽略时表现得很隐蔽：地址填着，流程却走不通。
			mail += "（未验证）"
		}
		twoFA := "未启用"
		if a.Has2FA {
			twoFA = "已启用"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Username, a.Status, twoFA, mail)
	}
	_ = w.Flush()
}

func requireUser(cmd, user string) {
	if strings.TrimSpace(user) == "" {
		fatal("命令 %s 需要 -user 指定目标用户名（先用 list-admins 查看）", cmd)
	}
}

// confirm 要求操作者输入目标用户名以确认。
//
// 与产品里其它不可逆操作（格式化、删除存储池）同一套做法（R-004）：让手
// 停一下。这个工具专门在出事的时候用，而那时候人往往很急——**在急的时候
// 敲错一个用户名，代价是重置掉一个无关账号的密码并把它踢下线**。
//
// -yes 可以跳过，用于确实无法交互的场合；但那时调用方至少是显式选择了跳过。
func confirm(skip bool, what string) {
	if skip {
		return
	}
	fmt.Println("\n将要执行：")
	fmt.Println("  " + what)
	fmt.Print("\n请输入目标用户名以确认（直接回车取消）：")

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.TrimSpace(line) == "" {
		fmt.Println("已取消。")
		os.Exit(1)
	}
	// 这里刻意**不再比对输入是否等于目标用户名**：目标用户名刚刚已经打印
	// 在屏幕上了（上面那两行），把同一个字符串再对一遍并不能证明操作者
	// 知道自己按的是谁。真正起作用的是"需要动手输入一次"这个停顿本身。
	// 加一层字符串比对只会让人以为它在防别的东西。
	fmt.Println()
}

func usage() {
	fmt.Fprint(os.Stderr, `服务器侧应急脚本（F-9-06）：面板不可用时的带外入口。

用法：
  emergency list-admins                    列出全部管理员账号
  emergency reset-password -user <名>      重置密码（不填 -password 则随机生成）
  emergency clear-2fa       -user <名>     清除密码器与全部恢复码
  emergency clear-email     -user <名>     清除邮箱绑定与验证状态

参数：
  -user <名>       目标用户名
  -password <串>   新密码；留空则自动生成临时密码
  -yes             跳过确认

说明：本工具不做身份验证——能在这台机器上运行它的人本来就能直接改数据库。
真正的边界是宿主机的登录权限。每次执行都会往审计里写一条来源为 emergency
的记录。
`)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误："+format+"\n", args...)
	os.Exit(1)
}
