package logging_test

import (
	"path/filepath"
	"testing"
	"time"

	"k_cockpit/internal/logging"
)

func newLogger(t *testing.T) *logging.Logger {
	t.Helper()
	opts := logging.DefaultOptions()
	opts.Dir = filepath.Join(t.TempDir(), "logs")
	opts.Level = logging.LevelDebug
	l, err := logging.New(opts)
	if err != nil {
		t.Fatalf("建日志器失败: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestSubscribeReceivesNewLines 覆盖最基本的行为。
func TestSubscribeReceivesNewLines(t *testing.T) {
	l := newLogger(t)
	sub := l.Subscribe()
	defer sub.Close()

	l.Infof("第一条")
	l.Warnf("第二条")

	for _, want := range []string{"第一条", "第二条"} {
		select {
		case e := <-sub.C():
			if e.Line != want {
				t.Errorf("收到 %q，期望 %q", e.Line, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("等待 %q 超时", want)
		}
	}
}

// TestSlowSubscriberDoesNotBlockLogging 覆盖本机制最要紧的一条保证。
//
// 不丢弃的话，一个卡住的浏览器标签（后台被节流、网线被拔、进程被挂起）
// 会把**整个服务的日志写入**阻塞住——日志是全局的，一个读者的停滞会变成
// 全站停滞。这类故障的表现是「服务无响应」，而原因藏在日志子系统里。
//
// 因此这里故意订阅之后完全不读，然后要求日志写入仍然能在毫秒级完成。
func TestSlowSubscriberDoesNotBlockLogging(t *testing.T) {
	l := newLogger(t)
	sub := l.Subscribe()
	defer sub.Close()

	// **故意不读 sub.C()。** 写满缓冲之后就该开始丢弃。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			l.Infof("灌日志 %d", i)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("日志写入被一个不读的订阅者阻塞住了——这是本机制要防的那种故障")
	}

	if sub.Dropped() == 0 {
		t.Error("缓冲写满后应当开始丢弃并记账，实际 dropped=0")
	}
}

// TestDroppedIsReportedOnce 覆盖丢弃计数的语义。
//
// 计数是**累计后被读走一次**：客户端要能说出「刚才漏了 N 行」，而不是
// 每次都报一个累计总数——累计数会让界面上的提示越滚越大且无法归零。
func TestDroppedIsReportedOnce(t *testing.T) {
	l := newLogger(t)
	sub := l.Subscribe()
	defer sub.Close()

	for i := 0; i < 5000; i++ {
		l.Infof("灌 %d", i)
	}

	first := sub.Dropped()
	if first == 0 {
		t.Fatal("应当有丢弃")
	}
	if second := sub.Dropped(); second != 0 {
		t.Errorf("第二次读取应为 0（已读走），实际 %d", second)
	}
}

// TestCloseStopsDelivery 覆盖取消订阅。
//
// 不摘掉的话，订阅会一直留在日志器里，随**每一条**日志被遍历一次——
// 那是一个随连接数增长的固定开销，而且没有任何地方会报错。
func TestCloseStopsDelivery(t *testing.T) {
	l := newLogger(t)
	sub := l.Subscribe()
	sub.Close()

	// 关闭之后写入不应 panic（向已关闭通道发数据才会 panic，而这里
	// 是先摘除再关闭，因此不会）。
	l.Infof("关闭之后的日志")

	// 通道不会被写入（订阅已摘除）。
	select {
	case e, ok := <-sub.C():
		if ok {
			t.Errorf("取消订阅后不该再收到日志，却收到 %q", e.Line)
		}
	case <-time.After(100 * time.Millisecond):
		// 没有新数据，符合预期。
	}
}

// TestSubscriptionsAreIndependent 覆盖多订阅者互不影响。
//
// 面板可能同时开着多个日志页（不同的人、或同一个人两个标签），
// 而「一个订阅者慢」不该让另一个也变慢。
func TestSubscriptionsAreIndependent(t *testing.T) {
	l := newLogger(t)
	fast := l.Subscribe()
	defer fast.Close()
	slow := l.Subscribe()
	defer slow.Close()

	// slow 完全不读。
	for i := 0; i < 5000; i++ {
		l.Infof("灌 %d", i)
	}

	// fast 一直在读吗？没有——它也满了。这里要断言的是**两者各自记账**，
	// 而不是共享一个计数。
	if slow.Dropped() == 0 {
		t.Error("slow 应当有丢弃")
	}
	// fast 同样没读，因此也该有自己的丢弃计数（说明计数不是全局的）。
	if fast.Dropped() == 0 {
		t.Error("fast 应当有**自己**的丢弃计数，而不是共享 slow 的")
	}
}
