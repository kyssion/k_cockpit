// qemuowner.go：agent 创建的、将被 qemu 进程打开的文件，属主与权限归一。
//
// 背景（真机实测）：agent 以 root 运行，qemu-img/virsh 创建的盘默认
// root:600；libvirt 的安全驱动只在**它自己**启动域时改属主——快照恢复
// （snapshot-revert --disk-only）新建的 overlay 不走那条路，留在 root:600，
// 下次开机 qemu（libvirt-qemu:kvm）打不开，报 Permission denied。
//
// 遵循 AGENTS.md「系统资源操作：探测 + 回退」：QEMU 属主在不同发行版是
// libvirt-qemu:kvm（Debian/Ubuntu）或 qemu:qemu（RHEL 系）——不硬编码，
// 从 libvirt 自己的运行目录探测，失败则回退到名字解析，再失败不改（保持
// root 属主，由 libvirt 的安全驱动在启动时接管——那是既有路径）。
package nodeagent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"k_cockpit/internal/nodeagent/sys"
)

// qemuOwner 缓存探测结果（0 表示未知/不改）。
var (
	qemuUID, qemuGID int
	qemuOwnerProbed  bool
)

// ensureQemuReadable 把 path 的属主改成 qemu 用户、权限 0644。
//
// 幂等：已是目标属主时只查权限。失败只返回错误由调用方决定语义（多数
// 场景下降级告警，不阻断——libvirt 启动时会再尝试接管）。
func ensureQemuReadable(path string) error {
	probeQemuOwner()
	if qemuUID == 0 || qemuGID == 0 {
		_ = os.Chmod(path, 0o644) // 探测不到也至少放开「其它用户可读」
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == qemuUID && int(st.Gid) == qemuGID {
		return os.Chmod(path, 0o644)
	}
	if err := os.Chown(path, qemuUID, qemuGID); err != nil {
		_ = os.Chmod(path, 0o644)
		return err
	}
	return os.Chmod(path, 0o644)
}

// probeQemuOwner 探测 qemu 运行用户与组。
//
// 优先级：libvirt qemu 运行目录的属主（/var/lib/libvirt/qemu，发行版安装
// 时就归属正确）→ 用户名解析（libvirt-qemu / qemu 二选一）→ 放弃。
func probeQemuOwner() {
	if qemuOwnerProbed {
		return
	}
	qemuOwnerProbed = true
	if fi, err := os.Stat("/var/lib/libvirt/qemu"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid > 0 {
			qemuUID, qemuGID = int(st.Uid), int(st.Gid)
			return
		}
	}
	for _, name := range []string{"libvirt-qemu", "qemu"} {
		if out, err := runID(name); err == nil {
			parts := strings.Fields(out)
			if len(parts) >= 2 {
				if uid, e1 := strconv.Atoi(parts[0]); e1 == nil {
					if gid, e2 := strconv.Atoi(parts[1]); e2 == nil && uid > 0 {
						qemuUID, qemuGID = uid, gid
						return
					}
				}
			}
		}
	}
}

// runID 解析用户的 uid 与 gid（getent 一次取 passwd 行，字段 3/4）。
func runID(name string) (string, error) {
	ctx, cancel := contextWithTimeout(3 * timeSecond)
	defer cancel()
	out, err := sys.Run(ctx, "getent", "passwd", name)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 4 {
		return "", fmt.Errorf("passwd 行字段不足: %q", out)
	}
	return fields[2] + " " + fields[3], nil
}

// ensureAppArmorAllowsImages 把镜像目录加进 libvirt 的 AppArmor 抽象层。
//
// 真机实测（Ubuntu 24.04 + libvirt 10）：动态 profile 不会为我们 XML 里的
// 非 /var/lib/libvirt 路径生成 .files 放行（QVMConsole 的 pitfalls 踩过同
// 款）——qemu 报 Permission denied 而属主权限完全正常。把目录写进
// abstractions/libvirt-qemu（每个域的 profile 都 include 它）是发行版通用
// 解法。幂等：已含**该目录**的标记行则跳过；apparmor 不存在则空操作。
func ensureAppArmorAllowsImages(dir string) {
	const abstraction = "/etc/apparmor.d/abstractions/libvirt-qemu"
	b, err := os.ReadFile(abstraction)
	if err != nil {
		return // 无 apparmor（或路径不同）——无需处理。
	}
	// 幂等必须按**目录**判定：只判「出现过 kc.images 标记」会让首个放行的
	// 目录吞掉后续所有调用（ISO 的 userdata、快照链的其它池目录永远写不进）。
	marker := "# kc.images:" + dir
	if strings.Contains(string(b), marker) {
		return
	}
	f, err := os.OpenFile(abstraction, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString("  " + dir + "/** rwk, " + marker + "\n")
	// 重载由下次 profile 更新自然携带；这里不主动 reload——agent 可能在
	// 域运行期间调用，reload 抽象层会牵连全部域 profile。
}
