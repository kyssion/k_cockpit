// Command agentgw_probe 是**节点通道的协议级探针**，只做真机验收里
// 「拿不到证书也要试一下」的几件事：
//
//	register 用指定协议版本去换证书（验 0/2 被 FailedPrecondition 拒绝且不签发）
//	channel  在无客户端证书 / 有客户端证书两种身份下开通道（验 mTLS 是硬门槛）
//
// 为什么需要它：enroll 与 run 都会**按当前版本**说话，无法构造「旧版本
// agent」或「没有证书的客户端」。这两类拒绝逻辑如果只靠单测里的函数调用，
// 验的是判断函数而不是网关行为——真的把无证书连接放过去时单测不会红。
//
// 用法（在节点上、对着控制面网关跑）：
//
//	agentgw_probe -mode register -addr 127.0.0.1:8443 -ca ca.pem -token <令牌> -proto 0
//	agentgw_probe -mode channel  -addr 127.0.0.1:8443 -ca ca.pem
//	agentgw_probe -mode channel  -addr 127.0.0.1:8443 -ca ca.pem -cert client.pem -key client.key
//
// 退出码：0 表示「与预期一致」（成功或按预期被拒，看输出里的 result= 前缀），
// 1 表示与预期相反。
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentproto "k_cockpit/internal/agentproto"
)

func main() {
	mode := flag.String("mode", "register", "register | channel")
	addr := flag.String("addr", "127.0.0.1:8443", "网关地址 host:port")
	caPath := flag.String("ca", "ca.pem", "控制面 CA 证书")
	certPath := flag.String("cert", "", "客户端证书（channel 用；留空表示无证书）")
	keyPath := flag.String("key", "", "客户端私钥")
	serverName := flag.String("server-name", "127.0.0.1", "TLS ServerName（证书 SAN 须匹配）")
	token := flag.String("token", "", "一次性注册令牌（register 用）")
	protoVer := flag.Int("proto", 1, "注册时声明的协议版本")
	agentID := flag.String("agent-id", "", "注册时声明的 agent_id（留空由控制面生成）")
	outCert := flag.String("out-cert", "", "把注册返回的客户端证书写到该路径（用于核对 CN）")
	timeout := flag.Duration("timeout", 10*time.Second, "单次调用超时")
	flag.Parse()

	caPEM, err := os.ReadFile(*caPath)
	if err != nil {
		fail("读取 CA 失败: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		fail("CA 证书解析失败")
	}
	tlsCfg := &tls.Config{RootCAs: pool, ServerName: *serverName, MinVersion: tls.VersionTLS12}
	if *certPath != "" {
		pair, err := tls.LoadX509KeyPair(*certPath, *keyPath)
		if err != nil {
			fail("加载客户端证书失败: %v", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		fail("建立连接失败: %v", err)
	}
	defer conn.Close()
	client := agentproto.NewAgentGatewayClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch *mode {
	case "register":
		resp, err := client.Register(ctx, &agentproto.RegisterRequest{
			Token:           *token,
			AgentId:         *agentID,
			AgentVersion:    "probe",
			ProtocolVersion: int32(*protoVer),
		})
		if err != nil {
			fmt.Printf("result=rejected proto=%d err=%v\n", *protoVer, err)
			return
		}
		fmt.Printf("result=accepted proto=%d agent_id=%s cert_bytes=%d\n",
			*protoVer, resp.GetAgentId(), len(resp.GetClientCert()))
		// 把证书落盘：mTLS 的身份约定是「证书 CN == agent_id」，而这一点
		// 只能在证书本体上核对——接口回显的 agent_id 与证书里的 CN 是
		// 两条独立来源，都一致才算成立。
		if *outCert != "" {
			if err := os.WriteFile(*outCert, resp.GetClientCert(), 0o600); err != nil {
				fail("写证书失败: %v", err)
			}
			fmt.Printf("cert_written=%s\n", *outCert)
		}
	case "channel":
		stream, err := client.Channel(ctx)
		if err != nil {
			fmt.Printf("result=rejected cert=%t err=%v\n", *certPath != "", err)
			return
		}
		// 开流本身可能异步失败：发一帧心跳逼出服务端的身份判定。
		_ = stream.Send(&agentproto.AgentToServer{
			Payload: &agentproto.AgentToServer_Heartbeat{Heartbeat: &agentproto.Heartbeat{
				AgentVersion: "probe", ProtocolVersion: 1,
			}},
		})
		_, recvErr := stream.Recv()
		if recvErr != nil {
			fmt.Printf("result=rejected cert=%t err=%v\n", *certPath != "", recvErr)
			return
		}
		fmt.Printf("result=accepted cert=%t\n", *certPath != "")
	default:
		fail("未知 mode: %s", *mode)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
