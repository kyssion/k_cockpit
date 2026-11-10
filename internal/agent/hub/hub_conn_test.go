package hub

import "testing"

// 同一节点起第二条通道时，旧通道必须被显式踢下线。
//
// 只把注册表里的指针换成新连接是不够的：旧流的读循环还阻塞在 Recv 上，
// 如果它比新流活得久，detach 就会把「已经不属于自己」的登记删掉，节点在
// 控制面上从此无连接——而它的 agent 仍在心跳。真机上是这样复现的：
// 用同一张客户端证书开一次探测通道，探测退出后在线节点变成不可达。
func TestAttachSupersedesOldConn(t *testing.T) {
	h := &Hub{conns: map[int64]*conn{}}

	old := &conn{nodeID: 1, pending: map[string]*pendingOp{}, superseded: make(chan struct{})}
	h.attach(old)

	newer := &conn{nodeID: 1, pending: map[string]*pendingOp{}, superseded: make(chan struct{})}
	h.attach(newer)

	select {
	case <-old.superseded:
	default:
		t.Fatal("旧连接未被踢下线：它的读循环会一直活着")
	}
	if got := h.connOf(1); got != newer {
		t.Fatalf("注册表应指向新连接，实际 %p", got)
	}

	// 新连接先断：必须把登记清掉，而不是留着一条死连接。
	h.detach(newer)
	if got := h.connOf(1); got != nil {
		t.Fatalf("新连接断开后不应残留登记，实际 %p", got)
	}

	// 旧连接随后自然结束，不能误删别人（此时表里已经是另一个新连接）。
	third := &conn{nodeID: 1, pending: map[string]*pendingOp{}, superseded: make(chan struct{})}
	h.attach(third)
	h.detach(old)
	if got := h.connOf(1); got != third {
		t.Fatalf("旧连接 detach 不应删除当前连接，实际 %p", got)
	}
}

// supersede 必须幂等：两次顶替（三条连接）不能 double close panic。
func TestSupersedeIsIdempotent(t *testing.T) {
	c := &conn{nodeID: 1, pending: map[string]*pendingOp{}, superseded: make(chan struct{})}
	c.supersede()
	c.supersede()
	select {
	case <-c.superseded:
	default:
		t.Fatal("superseded 未关闭")
	}
}
