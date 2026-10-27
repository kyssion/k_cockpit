// stream.go：VNC 字节流的控制面半边（f-2-08 R-013：流量不落盘、不进日志）。
//
// 链路：浏览器 WS → 控制台 handler → OpenStream 拿到 bridge 的**控制面端**
// → 下行 OpenStreamRequest → agent 回 StreamOpened(ok) 并回拨 StreamData
// RPC → 服务端把 RPC 的上下行各自泵进 bridge 的两端。任何一端关闭都会
// 拆掉整条链。
package hub

import (
	"context"
	"sync"

	"google.golang.org/grpc/metadata"

	"k_cockpit/internal/agent"
	agentproto "k_cockpit/internal/agentproto"
)

// streamChunkSize 是转发分片大小（32KB：VNC 帧更新常见量级，过大会增加
// 端到端延迟，过小则 RPC 帧头占比升高）。
const streamChunkSize = 32 * 1024

// streamBridge 是一条流的两侧桥。chToAgent 是控制面写、agent 读方向；
// chFromAgent 反向。
type streamBridge struct {
	id          string
	ctx         context.Context
	cancel      context.CancelFunc
	chToAgent   chan []byte
	chFromAgent chan []byte
	mu          sync.Mutex
	closed      bool
}

func newStreamBridge(id string) *streamBridge {
	ctx, cancel := context.WithCancel(context.Background())
	return &streamBridge{
		id: id, ctx: ctx, cancel: cancel,
		chToAgent:   make(chan []byte, 16),
		chFromAgent: make(chan []byte, 16),
	}
}

// close 拆桥：两端的读写都会立刻返回错误。
func (b *streamBridge) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.chToAgent)
	close(b.chFromAgent)
	b.cancel()
}

func (b *streamBridge) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// controlSide 把桥的一端包装成控制台 handler 期望的 Stream。
type controlSide struct {
	b    *streamBridge
	pend []byte // 上次 Read 没装下的剩余字节（丢了就是字节流中段丢失）
}

func (s *controlSide) Read(p []byte) (int, error) {
	// 先消费上次的剩余：VNC 是字节流，中段丢字节会直接破坏 RFB 协议。
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	chunk, ok := <-s.b.chFromAgent
	if !ok {
		return 0, errBridgeClosed
	}
	n := copy(p, chunk)
	if n < len(chunk) {
		s.pend = chunk[n:]
	}
	return n, nil
}

func (s *controlSide) Write(p []byte) (int, error) {
	if s.b.isClosed() {
		return 0, errBridgeClosed
	}
	total := len(p)
	// 分片拷贝：p 归调用方所有，异步转发必须持有副本。
	for len(p) > 0 {
		n := len(p)
		if n > streamChunkSize {
			n = streamChunkSize
		}
		chunk := make([]byte, n)
		copy(chunk, p[:n])
		select {
		case s.b.chToAgent <- chunk:
			p = p[n:]
		case <-s.b.ctx.Done():
			return 0, errBridgeClosed
		}
	}
	// 返回**原始**长度：循环结束后 p 已被切空，旧代码返回 len(p) 恒为 0，
	// 违反 io.Writer 契约（n<len(p) 且 err==nil），io.Copy 直接报错。
	return total, nil
}

func (s controlSide) Close() error { s.b.close(); return nil }

var errBridgeClosed = streamErr("stream: 已关闭")

type streamErr string

func (e streamErr) Error() string { return string(e) }

// OpenStream 实现 agent.StreamOpener（P0 的哨兵占位替换为真实转发）。
//
// 时序：登记等待者 → 下行 OpenStreamRequest → agent 回拨 StreamData RPC
// 并先送 StreamOpened(ok) → 收到 ok 后把 bridge 的 agent 端交给已到达的
// StreamData handler，返回控制面端。
func (h *Hub) OpenStream(ctx context.Context, kind agent.StreamKind, nodeID int64, target string) (agent.Stream, error) {
	if kind != agent.StreamVNC {
		return nil, agent.ErrStreamUnsupported
	}
	c := h.connOf(nodeID)
	if c == nil {
		return nil, streamErr("节点不在线，无法打开控制台流")
	}

	streamID := "s-" + newAgentID()
	bridge := newStreamBridge(streamID)
	waitCh := make(chan *streamOpenedMsg, 1)

	// **先登记 bridge 再下发打开指令**：StreamData 是另一条 RPC，与主通道
	// 上的 StreamOpened(ok) 没有顺序保证——旧代码等 ok 之后才登记，RPC
	// 先到时 takeAgentStream 拿到 nil，会话直接夭折。失败路径统一关桥。
	h.mu.Lock()
	if h.agentStreams == nil {
		h.agentStreams = map[string]*streamBridge{}
	}
	h.agentStreams[streamID] = bridge
	if c.pendingStreams == nil {
		c.pendingStreams = map[string]chan *streamOpenedMsg{}
	}
	c.pendingStreams[streamID] = waitCh
	h.mu.Unlock()
	go func() {
		<-bridge.ctx.Done()
		h.mu.Lock()
		delete(h.agentStreams, streamID)
		h.mu.Unlock()
	}()
	fail := func(err error) (agent.Stream, error) {
		bridge.close()
		return nil, err
	}
	defer func() {
		h.mu.Lock()
		delete(c.pendingStreams, streamID)
		h.mu.Unlock()
	}()
	defer func() {
		h.mu.Lock()
		delete(c.pendingStreams, streamID)
		h.mu.Unlock()
	}()

	if err := c.send(ctx, &serverToAgent{Payload: &agentproto.ServerToAgent_OpenStream{
		OpenStream: &agentproto.OpenStreamRequest{
			StreamId: streamID, Kind: string(kind), Target: target,
		},
	}}); err != nil {
		return fail(streamErr("流打开指令未送达: " + err.Error()))
	}

	select {
	case m := <-waitCh:
		if !m.Ok {
			return fail(streamErr(m.Message))
		}
	case <-ctx.Done():
		return fail(streamErr("打开控制台流超时"))
	}

	return &controlSide{b: bridge}, nil
}

// takeAgentStream 由 StreamData handler 调用：取走属于自己的桥。
func (h *Hub) takeAgentStream(id string) *streamBridge {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.agentStreams[id]
	delete(h.agentStreams, id)
	return b
}

// serveAgentStream 双向泵：agent 的 StreamData 流 ↔ 桥。
func (h *Hub) serveAgentStream(id string, stream agentproto.AgentGateway_StreamDataServer) {
	bridge := h.takeAgentStream(id)
	if bridge == nil {
		return
	}
	defer bridge.close()

	// agent → 控制面（读 RPC 推进 chFromAgent）。
	go func() {
		for {
			chunk, err := stream.Recv()
			if err != nil {
				bridge.close()
				return
			}
			if bridge.isClosed() {
				return
			}
			select {
			case bridge.chFromAgent <- chunk.Data:
			case <-bridge.ctx.Done():
				return
			}
		}
	}()

	// 控制面 → agent（chToAgent → RPC Send）。
	for {
		select {
		case <-bridge.ctx.Done():
			return
		case chunk, ok := <-bridge.chToAgent:
			if !ok {
				return
			}
			if err := stream.Send(&agentproto.StreamChunk{Data: chunk}); err != nil {
				return
			}
		}
	}
}

// streamIDFromContext 从 StreamData RPC 的 metadata 里取流标识。
func streamIDFromContext(md metadata.MD) string {
	if md == nil {
		return ""
	}
	v := md.Get("x-kc-stream-id")
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
