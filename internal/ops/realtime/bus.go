// Package realtime 提供一个进程内的事件总线，供 SSE 端点消费。
//
// 为什么需要它，而不是让前端继续轮询：任务与虚拟机的状态变化是**稀疏且
// 不可预测**的。轮询要么在大多数时候空转（浪费一次查询），要么把间隔压得
// 很短（在任务多时形成持续的写放大）。推送把"什么时候查"这件事交给真正
// 知道的一方——产生变化的那段代码。
//
// 三条刻意的取舍：
//
//  1. **进程内广播，不做跨实例分发**。当前部署是单实例；多实例时应当换
//     成数据库或消息队列做分发，而本包的接口（Publish / Subscribe）不需要
//     变。先把形状定对，将来替换实现不会波及调用方。
//  2. **订阅者慢就丢弃并计数**，而不是阻塞发布者。任务执行的 goroutine
//     不能因为某个浏览器标签页卡住而停下；丢了多少必须告诉前端（Dropped），
//     否则它会以为那段时间什么都没发生。
//  3. **事件只带"什么变了"，不带变化后的完整内容**。完整内容由前端拿到事件
//     后重新查询：推送一条可能过期的快照，比让客户端自己取一次准确的更糟。
package realtime

import (
	"sync"
	"sync/atomic"
	"time"
)

// 事件种类。
const (
	// KindTask 任务状态变化（入队 / 开始 / 落定）。
	KindTask = "task"
	// KindVM 虚拟机投影变化（创建成功、电源结果、删除）。
	KindVM = "vm"
)

// Event 描述一次变化。
type Event struct {
	Kind string `json:"kind"`
	// Status 是变化后的状态：任务为 pending / running / success / failed
	// / canceled，虚拟机为运行态。
	Status string `json:"status"`
	// ResourceType / ResourceID 定位到对象。任务事件里它们指向任务作用的
	// 资源（虚拟机、存储池…）而不是任务本身——前端关心的是"哪台机器变了"。
	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   int64  `json:"resource_id,omitempty"`
	ResourceName string `json:"resource_name,omitempty"`
	// OwnerID 用于按视角过滤：租户只收到自己名下资源的事件。
	OwnerID int64 `json:"owner_id,omitempty"`
	// TaskID 仅在任务事件里有意义，便于前端直接定位任务抽屉。
	TaskID int64     `json:"task_id,omitempty"`
	At     time.Time `json:"at"`
}

// Bus 是订阅关系的持有者。
type Bus struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
}

// NewBus 构造总线。
func NewBus() *Bus {
	return &Bus{subs: make(map[*Subscription]struct{})}
}

// Subscribe 订阅事件。返回的订阅**必须**由调用方 Close，否则它会一直留在
// 总线里被投递。
func (b *Bus) Subscribe() *Subscription {
	s := &Subscription{
		ch:   make(chan Event, subscriptionBuffer),
		done: make(chan struct{}),
		bus:  b,
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Publish 广播一个事件。
//
// 没有订阅者时直接返回：构造 Event 的代价虽然小，但任务的关键路径上不该
// 有任何与"有没有人在看"相关的固定开销。
func (b *Bus) Publish(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		s.deliver(e)
	}
}

// Subscribers 返回当前订阅数，供可观测性与测试使用。
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// subscriptionBuffer 是每个订阅者的缓冲深度。
//
// 它吸收"瞬间来了几条事件而客户端还没读"的情况；再大也只是把丢弃推迟，
// 不能替代背压，因此取一个小数。
const subscriptionBuffer = 32

// Subscription 是一个订阅者。
type Subscription struct {
	ch      chan Event
	done    chan struct{}
	bus     *Bus
	closeMu sync.Mutex
	closed  bool
	dropped int64
}

// C 返回事件通道。
func (s *Subscription) C() <-chan Event { return s.ch }

// Done 在订阅关闭时关闭。
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Dropped 返回自上次读取以来被丢弃的事件数，读完即清零。
func (s *Subscription) Dropped() int64 { return atomic.SwapInt64(&s.dropped, 0) }

// Close 取消订阅。可重复调用：SSE 的写协程与 defer 两条路径都可能触发。
func (s *Subscription) Close() {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	if s.bus != nil {
		s.bus.mu.Lock()
		delete(s.bus.subs, s)
		s.bus.mu.Unlock()
	}
	close(s.done)
}

// deliver 非阻塞投递；通道满时丢弃并计数。
func (s *Subscription) deliver(e Event) {
	select {
	case s.ch <- e:
	default:
		atomic.AddInt64(&s.dropped, 1)
	}
}
