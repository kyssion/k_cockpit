package nodeagent

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"k_cockpit/internal/agent"
	agentproto "k_cockpit/internal/agentproto"
)

// capturingConn 是实现 grpc.ClientConnInterface 的替身：它记下 StreamData
// 拿到的 ctx，然后让 RecvMsg 阻塞到该 ctx 结束——也就是「一条活着的流」。
//
// 用替身而不是真网关：这条缺陷的判据是**ctx 的生命周期归属**，用真连接
// 无法在单测里观察它。
type capturingConn struct {
	mu     sync.Mutex
	rpcCtx context.Context
}

func (c *capturingConn) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return nil
}

func (c *capturingConn) NewStream(
	ctx context.Context, _ *grpc.StreamDesc, _ string, _ ...grpc.CallOption,
) (grpc.ClientStream, error) {
	c.mu.Lock()
	c.rpcCtx = ctx
	c.mu.Unlock()
	return &blockingClientStream{ctx: ctx}, nil
}

func (c *capturingConn) captured() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rpcCtx
}

type blockingClientStream struct{ ctx context.Context }

func (s *blockingClientStream) Header() (metadata.MD, error) { return nil, nil }
func (s *blockingClientStream) Trailer() metadata.MD         { return nil }
func (s *blockingClientStream) CloseSend() error             { return nil }
func (s *blockingClientStream) Context() context.Context     { return s.ctx }
func (s *blockingClientStream) SendMsg(any) error            { return nil }

func (s *blockingClientStream) RecvMsg(any) error {
	<-s.ctx.Done()
	return s.ctx.Err()
}

// listenFakeVNC 占一个 5900+display 的端口并让假 virsh 报出对应显示号。
//
// vncAddr 解析的是 `virsh vncdisplay` 的输出（:0 → 5900），所以这里从 5900
// 往上找一个能监听的端口，让 dialVNC 真的连上来——被验证的是 ctx 的归属，
// 没必要把 dialVNC 也抽象掉。
func listenFakeVNC(t *testing.T) (display int, conns *[]net.Conn) {
	t.Helper()
	held := &[]net.Conn{}
	for n := 0; n < 64; n++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 5900+n))
		if err != nil {
			continue
		}
		var mu sync.Mutex
		go func() {
			for {
				c, aerr := ln.Accept()
				if aerr != nil {
					return
				}
				// 端到端"活着"的 VNC：连接保持打开，测试结束时统一关。
				mu.Lock()
				*held = append(*held, c)
				mu.Unlock()
			}
		}()
		t.Cleanup(func() {
			_ = ln.Close()
			mu.Lock()
			for _, c := range *held {
				_ = c.Close()
			}
			mu.Unlock()
		})
		return n, held
	}
	t.Fatal("找不到可监听的 5900+display 端口")
	return 0, nil
}

// TestHandleOpenStreamOutlivesDialCtx 覆盖「101 之后 0 字节」。
//
// 实机现象：WS 升级成功，控制台一个字节都收不到，会话在同一毫秒建立又结束；
// 而节点上直接 `bash /dev/tcp/127.0.0.1:5902` 能读到 `RFB 003.008`。
//
// 根因：回拨 StreamData RPC 的 ctx 派生自 dialCtx（3 秒拨号超时），而接收
// 循环在 handleOpenStream 返回后立刻 `dialCancel()` → RPC 立即被取消 →
// 控制面 Recv 立刻 EOF。这条用例把它钉死：
//  1. dial 取消**之后**，回拨 RPC 的 ctx 必须还活着；
//  2. 会话 ctx 取消时，它必须结束（否则流会泄漏到进程退出）。
func TestHandleOpenStreamOutlivesDialCtx(t *testing.T) {
	display, _ := listenFakeVNC(t)
	installFakeCommands(t, map[string]string{
		"virsh": fmt.Sprintf("#!/bin/sh\necho ':%d'\n", display),
	})

	conn := &capturingConn{}
	d := &Daemon{conn: conn}

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	// 用 WithCancel 而不是 WithTimeout(3s)：本用例断言的是「拨号 ctx 取消后
	// 回拨 RPC 还活着」，与拨号窗口长短无关。写成 3s 会让首次执行
	// t.TempDir() 里那个新建的假 virsh 脚本时被系统扫描（macOS 首次 exec
	// 较慢）误杀成 signal: killed，表现为随机失败。
	dialCtx, dialCancel := context.WithCancel(context.Background())

	out := d.handleOpenStream(streamCtx, dialCtx, &agentproto.OpenStreamRequest{
		StreamId: "s-1", Kind: string(agent.StreamVNC), Target: "demo-vm",
	})
	if out == nil || out.GetStreamOpened() == nil || !out.GetStreamOpened().Ok {
		t.Fatalf("应答应为 StreamOpened(ok=true)，实际: %+v", out)
	}

	rpcCtx := conn.captured()
	if rpcCtx == nil {
		t.Fatal("没有发起 StreamData RPC")
	}
	// 模拟 daemon 接收循环：handleOpenStream 一返回就取消拨号 ctx。
	dialCancel()
	if err := rpcCtx.Err(); err != nil {
		t.Fatalf("拨号 ctx 取消后回拨 RPC 也被取消了（%v）：控制面会立刻 EOF，控制台 101 后零字节", err)
	}

	// 反向：会话结束必须把流收掉，不能让它活到进程退出。
	streamCancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rpcCtx.Err() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("会话 ctx 取消后回拨 RPC 仍未结束（流泄漏）")
}

// TestHandleOpenStreamRejectsUnknownKind 覆盖类型护栏：不认识的流类型必须在
// 拨号之前就拒掉，而不是去连一个没有的 VNC 端口。
func TestHandleOpenStreamRejectsUnknownKind(t *testing.T) {
	d := &Daemon{conn: &capturingConn{}}
	out := d.handleOpenStream(context.Background(), context.Background(),
		&agentproto.OpenStreamRequest{StreamId: "s-2", Kind: "banana", Target: "demo-vm"})
	if out == nil || out.GetStreamOpened() == nil {
		t.Fatalf("应答应为 StreamOpened，实际: %+v", out)
	}
	if out.GetStreamOpened().Ok {
		t.Fatal("未知流类型必须拒绝")
	}
}
