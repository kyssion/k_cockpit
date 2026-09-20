package realtime_test

import (
	"testing"
	"time"

	"k_cockpit/internal/realtime"
)

func TestPublishReachesSubscriber(t *testing.T) {
	bus := realtime.NewBus()
	sub := bus.Subscribe()
	defer sub.Close()

	bus.Publish(realtime.Event{Kind: realtime.KindTask, Status: "running", TaskID: 7})

	select {
	case got := <-sub.C():
		if got.Kind != realtime.KindTask || got.TaskID != 7 {
			t.Errorf("事件 = %+v", got)
		}
		if got.At.IsZero() {
			t.Error("未填时间：前端要靠它判断事件的新旧")
		}
	case <-time.After(time.Second):
		t.Fatal("订阅者没有收到事件")
	}
}

// TestSlowSubscriberDropsInsteadOfBlocking 覆盖背压策略。
//
// 任务执行的 goroutine 不能因为某个浏览器标签页卡住而停下，因此慢订阅者
// 会被丢弃；而丢了多少必须可查——不说的话前端会以为那段时间什么都没发生。
func TestSlowSubscriberDropsInsteadOfBlocking(t *testing.T) {
	bus := realtime.NewBus()
	sub := bus.Subscribe()
	defer sub.Close()

	// 不读通道，灌满缓冲之后再多发几条。
	for i := 0; i < 200; i++ {
		bus.Publish(realtime.Event{Kind: realtime.KindTask, TaskID: int64(i)})
	}
	if n := sub.Dropped(); n == 0 {
		t.Error("缓冲溢出后应当记录丢弃数")
	}
	// 读一次之后计数清零，避免同一个缺口被反复上报。
	if n := sub.Dropped(); n != 0 {
		t.Errorf("读取后丢弃数 = %d, 期望 0", n)
	}
}

// TestCloseRemovesSubscriber 覆盖"关闭后不再收到事件"。
//
// 断开的连接如果不从总线里移除，它会一直占用一次非阻塞发送——而随着时间
// 推移，总线会越来越慢，表现为"界面更新越来越迟钝"。
func TestCloseRemovesSubscriber(t *testing.T) {
	bus := realtime.NewBus()
	sub := bus.Subscribe()
	if bus.Subscribers() != 1 {
		t.Fatalf("订阅数 = %d, 期望 1", bus.Subscribers())
	}
	sub.Close()
	if bus.Subscribers() != 0 {
		t.Errorf("关闭后订阅数 = %d, 期望 0", bus.Subscribers())
	}
	select {
	case <-sub.Done():
	default:
		t.Error("Done 通道未关闭")
	}
	// 重复关闭必须是安全的：SSE 的写协程与 defer 两条路径都会触发。
	sub.Close()
}

// TestPublishWithoutSubscriberIsCheap 覆盖没人订阅时的行为。
func TestPublishWithoutSubscriberIsCheap(t *testing.T) {
	bus := realtime.NewBus()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bus.Publish(realtime.Event{Kind: realtime.KindVM})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("无订阅者时 Publish 阻塞了")
	}
}
