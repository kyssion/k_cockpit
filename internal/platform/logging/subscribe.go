package logging

import (
	"sync"
	"sync/atomic"
)

// Subscription 是一条日志订阅（F-9-02 的实时查看）。
//
// 它的存在是为了让「日志在线查看」不必靠轮询：那是一种**只增**的数据流，
// 而用户打开这一页的原因通常就是盯着一件事的发生——轮询在这里的两个缺点
// 都成立（最长一个间隔的延迟，以及无人看着时也照轮）。
type Subscription struct {
	ch chan Entry

	// dropped 记录因消费不及时而被丢弃的行数。
	//
	// **丢弃是必须的，而且不能让丢弃变成静默的。**
	//
	// 不丢弃的话，一个卡住的浏览器标签（后台被节流、网线被拔、进程被挂起）
	// 会把整个服务的日志写入阻塞住——日志是全局的，一个读者的停滞会变成
	// 全站停滞。这类故障的表现是「服务无响应」，而原因藏在日志子系统里。
	//
	// 但丢弃本身也必须被说出来：不说的话，客户端会以为「这段时间没有日志」，
	// 而实际上有、只是没送达。界面上要能看到这个缺口。
	dropped atomic.Int64

	// logger 用于在 Close 时**同步**把自己摘掉。
	logger *Logger

	closeOnce sync.Once
	closed    chan struct{}
}

// C 是订阅的数据通道。通道关闭表示订阅已被取消。
func (s *Subscription) C() <-chan Entry { return s.ch }

// Dropped 返回自上次读取以来被丢弃的行数，并清零。
func (s *Subscription) Dropped() int64 { return s.dropped.Swap(0) }

// Close 取消订阅。可重复调用。
//
// **摘除与关闭通道都在日志器的锁内同步完成**，因此 Close 返回之后就绝不会
// 再收到任何一行。
//
// 这一点最初写错了：那时用一个 goroutine 异步摘除，理由是「Close 不该阻塞
// 在日志器的锁上」。而那是错的——锁只在写入的瞬间被持有，而 Close 本来就不是
// 热路径；正确的做法是让**正确性**优先。测试直接抓到了它：Close 之后写入的
// 那一行仍然被送进了通道。
//
// 在锁内 close(ch) 是安全的：publishLocked 也持同一把锁，两者因此被串行化，
// 不可能出现「向已关闭的通道发送」。
func (s *Subscription) Close() {
	s.closeOnce.Do(func() {
		s.logger.mu.Lock()
		delete(s.logger.subs, s)
		close(s.ch)
		s.logger.mu.Unlock()
		close(s.closed)
	})
}

// Done 在订阅被取消时关闭。
func (s *Subscription) Done() <-chan struct{} { return s.closed }

// subBuffer 是每个订阅的缓冲深度。
//
// 取值考虑：一次日志突增（例如某处循环里打日志）可能在几毫秒内产生几百行，
// 而浏览器的接收是逐个事件渲染的。太小会让正常的突增也被判成「消费不及时」，
// 太大则让一个已经卡住的读者占用更多内存——每条日志几百字节，1024 条约几百 KB，
// 对每一条订阅是可接受的。
const subBuffer = 1024

// Subscribe 新增一条订阅。调用方**必须**在结束时 Close，否则订阅会一直留在
// 日志器里，随每条日志被遍历一次。
func (l *Logger) Subscribe() *Subscription {
	sub := &Subscription{
		ch:     make(chan Entry, subBuffer),
		closed: make(chan struct{}),
		logger: l,
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.subs == nil {
		l.subs = map[*Subscription]struct{}{}
	}
	l.subs[sub] = struct{}{}
	return sub
}

// publishLocked 把一条日志推给全部订阅。调用方须持有锁。
//
// **非阻塞投递**：任何一个订阅满了就丢弃并记账，绝不等待。等待会让日志写入
// （进而让所有调用 log 的代码路径）停在一个浏览器标签的状态上。
func (l *Logger) publishLocked(e Entry) {
	for sub := range l.subs {
		select {
		case sub.ch <- e:
		default:
			sub.dropped.Add(1)
		}
	}
}
