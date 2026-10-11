// Command agent 是节点侧代理二进制（f-6-08）。
//
// 两个子命令：
//
//	k_cockpit-agent enroll -server host:port -token XXX   # 一次性注册，换客户端证书
//	k_cockpit-agent run                                   # 常驻：连接、心跳、执行指令
//
// 生产形态：systemd 托管 `run`；enroll 是部署时的手工/脚本步骤。两者都
// 不读取环境变量里的秘密——身份材料全部来自 enroll 换来的证书文件。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/google/uuid"

	"k_cockpit/internal/nodeagent"
)

const componentName = "k_cockpit-agent"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "enroll":
		err = cmdEnroll(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "version":
		fmt.Printf("k_cockpit-agent %s (protocol %d, %s/%s)\n",
			nodeagent.AgentVersionOf(), nodeagent.ProtocolVersion, runtime.GOOS, runtime.GOARCH)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("[%s] %v", componentName, err)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `k_cockpit-agent — K Cockpit 节点代理

用法：
  k_cockpit-agent enroll -server host:port -token TOKEN [-state-dir DIR] [-id UUID]
  k_cockpit-agent run    [-state-dir DIR] [-server HOST:PORT]
  k_cockpit-agent version

说明：
  enroll 用控制面签发的一次性注册令牌换取客户端证书（mTLS 身份），
  写入 state-dir 后即可 run。run 常驻连接控制面：10s 心跳、接收并执行
  领域操作、断线按指数退避重连。
`)
}

// cmdEnroll 执行一次性注册。
//
// -id 缺省生成 uuid 并持久化：删掉 state-dir 重装会换新身份——那是「这是
// 一台新节点」的语义；要保留身份与历史，保留 state-dir 或显式 -id。
func cmdEnroll(args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "控制面网关地址（host:port），例如 192.168.64.1:8443")
	token := fs.String("token", "", "一次性注册令牌（面板 → 节点 → 注册令牌 生成）")
	stateDir := fs.String("state-dir", "/var/lib/k_cockpit-agent", "身份与证书目录")
	id := fs.String("id", "", "agent 身份（缺省生成 uuid）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *server == "" || *token == "" {
		return fmt.Errorf("-server 与 -token 必填")
	}

	sd := nodeagent.StateDir{Dir: *stateDir}
	agentID := *id
	if agentID == "" {
		agentID = uuid.NewString()
	}

	result, err := nodeagent.Enroll(context.Background(), nodeagent.EnrollInput{
		Server:  *server,
		Token:   *token,
		AgentID: agentID,
		Version: nodeagent.AgentVersionOf(),
		TLS:     sd.EnrollTLS(),
	})
	if err != nil {
		return err
	}
	if err := sd.SaveEnrollment(&result.State, result.CaPEM, result.CertPEM, result.KeyPEM); err != nil {
		return fmt.Errorf("保存身份材料: %w", err)
	}
	log.Printf("[%s] 已注册为节点 %q（agent_id=%s），证书写入 %s；运行 `k_cockpit-agent run` 启动常驻通道",
		componentName, result.NodeName, result.State.AgentID, *stateDir)
	return nil
}

// cmdRun 常驻运行。
func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	stateDir := fs.String("state-dir", "/var/lib/k_cockpit-agent", "身份与证书目录")
	// -server 允许临时覆盖 agent.json 里的地址（调试换端口用，不落盘）。
	server := fs.String("server", "", "覆盖已保存的控制面地址")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sd := nodeagent.StateDir{Dir: *stateDir}
	st, err := sd.Load()
	if err != nil {
		return fmt.Errorf("尚未 enroll 或身份损坏（先运行 enroll 子命令）: %w", err)
	}
	addr := st.Server
	if *server != "" {
		addr = *server
	}
	tlsConf, err := sd.ClientTLS()
	if err != nil {
		return err
	}

	log.Printf("[%s] 启动：agent_id=%s server=%s version=%s protocol=%d",
		componentName, st.AgentID, addr, nodeagent.AgentVersionOf(), nodeagent.ProtocolVersion)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := &nodeagent.Daemon{
		Server:       addr,
		AgentID:      st.AgentID,
		AgentVersion: nodeagent.AgentVersionOf(),
		Hostname:     hostname(),
		TLS:          tlsConf,
		Registry:     nodeagent.NewRegistry(),
	}
	return d.Run(ctx)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
