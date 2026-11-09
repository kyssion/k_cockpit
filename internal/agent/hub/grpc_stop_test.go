package hub

// 停机路径的回归测试。
//
// 背景（实机缺陷，2026-11-09）：控制面收到 SIGTERM 后调 Hub.Stop →
// grpc.Server.GracefulStop，而 agent 的 Channel 是长连接双向流，正常运行时
// 永不结束。GracefulStop 于是永不返回，进程存活 22 分钟、继续持有 18443 的
// ESTABLISHED 连接并继续写心跳，新实例的连接表为空 → 所有指令送不达。
//
// 这些用例用假 stopper 复现「GracefulStop 卡住」的形状：一旦有人把
// stopGateway 改回直接调用 GracefulStop，第一个用例会以超时失败。

import (
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// 编译期断言：真实 grpc.Server 必须满足 helper 的接口收窄，否则 Stop 里
// 的调用点会以「装配 bug」的形式在编译期暴露，而不是在停机时才炸。
var _ grpcStopper = (*grpc.Server)(nil)

// watchdog 是「helper 该返回却没返回」的判定上限，同时兼作测试超时保护：
// 没有它，缺陷回归会表现为整包测试挂死而不是一条失败。
const watchdog = 5 * time.Second

// hangingStopper 模拟「GracefulStop 永不返回，直到 Stop 被调用」。
//
// 这正是长连接双向流在真实 grpc.Server 上的行为：quit 已触发、监听已关，
// 但传输不 drain，条件变量的等待永远醒不来。
type hangingStopper struct {
	stopCalled chan struct{}
	stopOnce   sync.Once

	mu            sync.Mutex
	gracefulCalls int
	stopCalls     int
}

func newHangingStopper() *hangingStopper {
	return &hangingStopper{stopCalled: make(chan struct{})}
}

func (f *hangingStopper) GracefulStop() {
	f.mu.Lock()
	f.gracefulCalls++
	f.mu.Unlock()
	<-f.stopCalled
}

func (f *hangingStopper) Stop() {
	f.mu.Lock()
	f.stopCalls++
	f.mu.Unlock()
	// close 只做一次：真实 grpc.Server 的 Stop 也是可重复调用的。
	f.stopOnce.Do(func() { close(f.stopCalled) })
}

func (f *hangingStopper) counts() (graceful, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gracefulCalls, f.stopCalls
}

// idleStopper 模拟「没有长连接、GracefulStop 立即返回」的干净停机。
type idleStopper struct {
	mu            sync.Mutex
	gracefulCalls int
	stopCalls     int
}

func (f *idleStopper) GracefulStop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gracefulCalls++
}

func (f *idleStopper) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
}

func (f *idleStopper) counts() (graceful, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gracefulCalls, f.stopCalls
}

// measureStop 在 watchdog 保护下运行 stopGateway，返回是否强制停止与耗时。
func measureStop(t *testing.T, gs grpcStopper, grace time.Duration) (forced bool, elapsed time.Duration) {
	t.Helper()
	type outcome struct {
		forced  bool
		elapsed time.Duration
	}
	start := time.Now()
	ch := make(chan outcome, 1)
	go func() {
		forced := stopGateway(gs, grace)
		ch <- outcome{forced: forced, elapsed: time.Since(start)}
	}()
	select {
	case o := <-ch:
		return o.forced, o.elapsed
	case <-time.After(watchdog):
		t.Fatalf("stopGateway 在 %s 内没有返回：GracefulStop 又被长连接卡住了（缺陷回归）", watchdog)
		return false, 0
	}
}

// TestStopGatewayReturnsAfterGraceWhenStreamsHang 是本缺陷的靶心用例：
// 在途流永不结束，helper 必须在 grace 之后强制停止并如实报告。
func TestStopGatewayReturnsAfterGraceWhenStreamsHang(t *testing.T) {
	const grace = 100 * time.Millisecond

	gs := newHangingStopper()
	forced, elapsed := measureStop(t, gs, grace)

	if !forced {
		t.Fatalf("长连接未结束时 stopGateway 应报告强制停止（true），实际 false")
	}
	if elapsed < grace {
		t.Errorf("耗时 %s 小于 grace %s：说明没等就强杀了，在途发送没有收尾窗口", elapsed, grace)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("耗时 %s 超过 2s：超时控制失效，停机仍然可能长时间不返回", elapsed)
	}
	gracefulCalls, stopCalls := gs.counts()
	if gracefulCalls != 1 {
		t.Errorf("GracefulStop 调用次数 = %d，期望 1", gracefulCalls)
	}
	if stopCalls != 1 {
		t.Errorf("Stop 调用次数 = %d，期望恰好 1", stopCalls)
	}
}

// TestStopGatewayPrefersGracefulWhenIdle 保证没有长连接时不会无谓地
// 强杀：正常停机不该把在途传输直接打断。
func TestStopGatewayPrefersGracefulWhenIdle(t *testing.T) {
	// grace 故意给得很长：若实现是「先睡 grace 再判断」，这条会因耗时超限失败。
	const grace = 30 * time.Second

	gs := &idleStopper{}
	forced, elapsed := measureStop(t, gs, grace)

	if forced {
		t.Errorf("GracefulStop 立即返回时不应报告强制停止，实际 true")
	}
	if elapsed >= 100*time.Millisecond {
		t.Errorf("耗时 %s >= 100ms：优雅停止成功后不应再等超时", elapsed)
	}
	gracefulCalls, stopCalls := gs.counts()
	if gracefulCalls != 1 {
		t.Errorf("GracefulStop 调用次数 = %d，期望 1", gracefulCalls)
	}
	if stopCalls != 0 {
		t.Errorf("Stop 调用次数 = %d，期望 0（不应强杀）", stopCalls)
	}
}

// TestStopGatewayIsIdempotentForRepeatedCalls 覆盖 main 里的重启路径可能
// 出现的重复调用：第二次调用时 GracefulStop 已因第一次的 Stop 而返回，
// 因此报告「非强制」且不再累计 Stop——不 panic、不重复强杀。
func TestStopGatewayIsIdempotentForRepeatedCalls(t *testing.T) {
	const grace = 100 * time.Millisecond

	gs := newHangingStopper()

	firstForced, _ := measureStop(t, gs, grace)
	if !firstForced {
		t.Fatalf("第一次调用应强制停止，实际 false")
	}
	secondForced, secondElapsed := measureStop(t, gs, grace)
	if secondForced {
		t.Errorf("第二次调用不应再次强制停止，实际 true")
	}
	if secondElapsed >= 100*time.Millisecond {
		t.Errorf("第二次调用耗时 %s：连接已清空，应立即返回", secondElapsed)
	}
	_, stopCalls := gs.counts()
	if stopCalls != 1 {
		t.Errorf("Stop 累计调用次数 = %d，期望 1", stopCalls)
	}
}

// TestHubStopBeforeServeIsNoop 覆盖「网关还没起来就收到 SIGTERM」：Serve
// 尚未登记 gs，Stop 必须是空操作而不是解引用 nil。
func TestHubStopBeforeServeIsNoop(t *testing.T) {
	h := &Hub{}
	h.Stop() // 不 panic 即通过；gs 为 nil 时 helper 不应被调用。
	if h.Online(1) {
		t.Errorf("未 Serve 的 Hub 不应报告任何节点在线")
	}
}
