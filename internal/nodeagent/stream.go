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
// 在 daemon 的接收循环里**同步执行**：StreamOpened 应答必须先于 StreamData
// RPC 到达控制面（OpenStream 的等待者先收到 ok，handler 再拿到桥）。
// 返回要经通道发出的应答。
func (d *Daemon) handleOpenStream(ctx context.Context, req *agentproto.OpenStreamRequest) *agentproto.AgentToServer {
	if agent.StreamKind(req.Kind) != agent.StreamVNC {
		return streamOpenedMsg(req.StreamId, false, "不支持的流类型: "+req.Kind)
	}
	conn, err := dialVNC(ctx, req.Target)
	if err != nil {
		return streamOpenedMsg(req.StreamId, false, err.Error())
	}

	// 回拨 StreamData：复用 daemon 持有的 gRPC 连接（同一 mTLS 通道）。
	rpcCtx, cancel := context.WithCancel(ctx)
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
	// 一侧断开，另一侧的 Read/Write 会随之失败退出。
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
	if i := strings.LastIndex(display, ":"); i > 0 {
		host = display[:i]
		portStr = display[i+1:]
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
