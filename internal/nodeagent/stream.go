// stream.go：agent 侧的 VNC 字节流（f-2-08 R-013：流量不落盘、不进日志）。
//
// 收到 OpenStreamRequest 后：解析域的 VNC 端口 → 拨通 → 回 StreamOpened(ok)
// → 发起 StreamData RPC（流标识经请求头）→ 双向泵。
package nodeagent

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"

	"google.golang.org/grpc/metadata"

	"k_cockpit/internal/agent"
	agentproto "k_cockpit/internal/agentproto"
	"k_cockpit/internal/nodeagent/sys"
)

// handleOpenStream 处理控制面的流打开请求。
//
// **两个 context 必须分开，不能合并**——这是控制台实机黑屏的根因，
// 别再"顺手"把它们收敛成一个：
//
//   - dialCtx 只管拨号。daemon 给它套了 3 秒超时，并且在**本函数返回后
//     立刻 cancel**（为了 VNC 端口异常时不阻塞接收循环）。
//   - streamCtx 管回拨的 StreamData RPC 的**整条寿命**（daemon 的会话级
//     ctx，控制面断线/agent 关停时才取消）。
//
// 曾经两者是同一个：`rpcCtx := context.WithCancel(ctx)`，而 ctx 就是那个
// 3 秒的 dialCtx。于是 handleOpenStream 一回 StreamOpened(ok)、接收循环
// 执行 dialCancel()，刚建立的 StreamData 立刻被取消 → 控制面 `Recv()`
// 立即 EOF → streamBridge 关闭 → 控制面 `stream.Read` 立刻返回
// 「stream: 已关闭」→ pipeConsole 结束。表现是：WS 101 之后零字节、
// 「会话建立」与「会话结束」在同一毫秒、而节点上直接
// `bash /dev/tcp/127.0.0.1:5902` 明明能读到 `RFB 003.008`。
//
// 在 daemon 的接收循环里**同步执行**：StreamOpened 应答必须先于 StreamData
// RPC 到达控制面（OpenStream 的等待者先收到 ok，handler 再拿到桥）。
// 返回要经通道发出的应答。
func (d *Daemon) handleOpenStream(
	streamCtx, dialCtx context.Context, req *agentproto.OpenStreamRequest,
) *agentproto.AgentToServer {
	if agent.StreamKind(req.Kind) != agent.StreamVNC {
		return streamOpenedMsg(req.StreamId, false, "不支持的流类型: "+req.Kind)
	}
	conn, err := dialVNC(dialCtx, req.Target)
	if err != nil {
		return streamOpenedMsg(req.StreamId, false, err.Error())
	}

	// 回拨 StreamData：复用 daemon 持有的 gRPC 连接（同一 mTLS 通道）。
	// rpcCtx 派生自 streamCtx（会话级），**不是** dialCtx——拨号超时不
	// 继承给长连接 RPC。
	//
	// 代价与取舍：接收循环「不被卡住」的保证现在只由 dialVNC 的 3 秒窗口
	// 承担。这不影响原意——StreamData 在一条**已建立**的 ClientConn 上创建
	// 时不会等待握手，失败会立刻返回（gRPC 默认不耐烦等 WaitForReady）；
	// 真正会等的是流建立之后的 Recv，那本来就在独立 goroutine 里。
	rpcCtx, cancel := context.WithCancel(streamCtx)
	rpcCtx = metadata.AppendToOutgoingContext(rpcCtx, "x-kc-stream-id", req.StreamId)
	gwClient := agentproto.NewAgentGatewayClient(d.conn)
	stream, err := gwClient.StreamData(rpcCtx)
	if err != nil {
		cancel()
		_ = conn.Close()
		return streamOpenedMsg(req.StreamId, false, "发起字节流失败: "+err.Error())
	}

	// 先应答，再泵数据——控制面 OpenStream 等到 ok 后才开始消费。
	out := streamOpenedMsg(req.StreamId, true, "")
	go func() {
		defer cancel()
		defer func() { _ = conn.Close() }()
		pumpVNC(stream, conn)
		log.Printf("[%s] 流 %s 结束（%s）", runAgentName, req.StreamId, req.Target)
	}()
	return out
}

// pumpVNC 双向泵：gRPC 流 ↔ VNC TCP 连接。
func pumpVNC(stream agentproto.AgentGateway_StreamDataClient, conn io.ReadWriteCloser) {
	done := make(chan struct{})

	// VNC → 控制面。
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if serr := stream.Send(&agentproto.StreamChunk{Data: buf[:n]}); serr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// 控制面 → VNC。
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		if _, werr := conn.Write(chunk.Data); werr != nil {
			break
		}
	}
	// 控制面侧已断：**立刻关 conn** 再等对侧。VM 空闲时 VNC 无数据可读，
	// 对侧协程会卡在 conn.Read——只靠 defer 的 Close（排在 <-done 之后）
	// 会死锁泄漏两个协程加一个 fd（每次空闲会话漏一份）。
	_ = conn.Close()
	<-done
}

func streamOpenedMsg(id string, ok bool, msg string) *agentproto.AgentToServer {
	return &agentproto.AgentToServer{Payload: &agentproto.AgentToServer_StreamOpened{
		StreamOpened: &agentproto.StreamOpened{StreamId: id, Ok: ok, Message: msg},
	}}
}

// dialVNC 拨通域的 VNC 端口。
func dialVNC(ctx context.Context, vm string) (net.Conn, error) {
	addr, err := vncAddr(ctx, vm)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// vncAddr 解析域的 VNC 监听地址（virsh vncdisplay）。
func vncAddr(ctx context.Context, vm string) (string, error) {
	out, err := sys.Run(ctx, "virsh", "-r", "-c", "qemu:///system", "vncdisplay", vm)
	if err != nil {
		return "", fmt.Errorf("读取 VNC 显示失败: %w", err)
	}
	display := strings.TrimSpace(out)
	host := "127.0.0.1"
	portStr := display
	// virsh vncdisplay 的两种常见输出：裸显示号 ":0"（LastIndex 命中 i=0，
	// 旧代码的 i>0 条件会跳过剥离，Atoi(":0") 必炸——VNC 对默认配置的
	// 域直接不可用）与 "host:port"。
	if i := strings.LastIndex(display, ":"); i >= 0 {
		portStr = display[i+1:]
		if i > 0 {
			host = strings.Trim(display[:i], "[]") // IPv6 字面量去括号
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}
	n, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil {
		return "", fmt.Errorf("VNC 显示号异常: %q", display)
	}
	// virsh 的显示号是偏移量（:0 → 5900）。
	return net.JoinHostPort(host, strconv.Itoa(5900+n)), nil
}
