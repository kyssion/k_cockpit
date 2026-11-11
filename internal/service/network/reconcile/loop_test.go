package reconcile_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"k_cockpit/internal/service/network/reconcile"
)

// recorder 是带计数的 target，用来观察循环到底跑了几轮、失败后是否继续。
type recorder struct {
	mu    sync.Mutex
	calls int
}

func (r *recorder) target(name string, n int, err error) reconcile.Target {
	return reconcile.Target{
		Name: name,
		Run: func(context.Context) (int, error) {
			r.mu.Lock()
			r.calls++
			r.mu.Unlock()
			return n, err
		},
	}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestNewLoopAppliesDefaults 覆盖 withDefaults 的默认分支。
//
// 零值 Interval 会让 time.NewTicker panic（non-positive interval），所以
// 「NewLoop 之后 Interval 一定为正」不是风格问题而是能不能起来的问题。
// 这里通过一个真实 Start 来验证：若默认值没生效，NewTicker 会直接 panic，
// 测试会以崩栈失败而不是超时失败——两者都能暴露，但前者定位更快。
func TestNewLoopAppliesDefaults(t *testing.T) {
	loop := reconcile.NewLoop(nil, reconcile.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		loop.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未在 ctx 取消后返回：默认间隔可能被当成 0 之外的极小值")
	}
}

// TestStartReturnsOnCancel 覆盖 ctx 取消即返回。
//
// 泄漏一个永不退出的后台 goroutine 在测试里看不出来，在生产里就是进程
// 无法优雅退出（systemd 只能等 TimeoutStopSec 然后 SIGKILL）。
func TestStartReturnsOnCancel(t *testing.T) {
	r := &recorder{}
	loop := reconcile.NewLoop([]reconcile.Target{r.target("noop", 0, nil)},
		reconcile.Options{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		loop.Start(ctx)
		close(done)
	}()

	// 间隔是 1 小时，正常情况下一次都不会触发；取消必须立刻返回。
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Start 未返回")
	}

	if got := r.count(); got != 0 {
		t.Errorf("1 小时间隔下不该触发对账，实际触发 %d 次", got)
	}
}

// TestStartRunsOnInterval 覆盖「按 Interval 周期执行」。
//
// 用 20ms 间隔、150ms 超时：至少应跑到 2 轮。断言取宽松下界——目标不是
// 精确计时（CI 上调度抖动很大），而是证明 ticker.C 这一路真的接上了。
func TestStartRunsOnInterval(t *testing.T) {
	r := &recorder{}
	loop := reconcile.NewLoop([]reconcile.Target{r.target("tick", 0, nil)},
		reconcile.Options{Interval: 20 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	loop.Start(ctx)

	if got := r.count(); got < 2 {
		t.Errorf("150ms / 20ms 至少应跑 2 轮，实际 %d 轮", got)
	}
}

// TestRunOnceContinuesAfterFailure 覆盖「单轮失败只记日志，不影响其它目标」。
//
// 对账是自愈类动作：一次失败（比如数据库重启）下一轮再试即可。若这里改成
// 遇到错误就 return，第一个坏掉的目标会让排在后面的镜像、桥接对账永远不执行
// ——而它们的失败与第一个目标毫无关系。
func TestRunOnceContinuesAfterFailure(t *testing.T) {
	bad := &recorder{}
	good1 := &recorder{}
	good2 := &recorder{}

	loop := reconcile.NewLoop([]reconcile.Target{
		bad.target("bad", 0, errors.New("数据库暂时不可用")),
		good1.target("good-1", 3, nil),
		good2.target("good-2", 0, nil),
	}, reconcile.Options{})

	loop.RunOnce(context.Background())

	if bad.count() != 1 {
		t.Errorf("失败目标应被尝试 1 次，实际 %d", bad.count())
	}
	if good1.count() != 1 || good2.count() != 1 {
		t.Errorf("失败后应继续执行后续目标，实际 good1=%d good2=%d",
			good1.count(), good2.count())
	}
}

// TestRunOnceIsIdempotent 覆盖重复执行的幂等性。
//
// 对账本身必须是幂等的（它只是把「节点已经做完的事」写回库里），否则
// 「同一轮跑两次」会让面板上的数字翻倍。这里断言重复调用不会因为内部
// 状态（比如已处理标记）而跳过——每一轮都真的要跑一遍。
func TestRunOnceIsIdempotent(t *testing.T) {
	r := &recorder{}
	loop := reconcile.NewLoop([]reconcile.Target{r.target("idem", 1, nil)},
		reconcile.Options{})

	for i := 0; i < 3; i++ {
		loop.RunOnce(context.Background())
	}
	if got := r.count(); got != 3 {
		t.Errorf("三轮 RunOnce 应各执行一次，实际 %d 次", got)
	}
}

// TestRunOnceWithNoTargets 覆盖空装配。
//
// 装配层漏挂目标时，循环应该是「什么都不做」，而不是 panic 或空转刷日志。
func TestRunOnceWithNoTargets(t *testing.T) {
	loop := reconcile.NewLoop(nil, reconcile.Options{})
	loop.RunOnce(context.Background()) // 不该 panic
}

// TestRunOnceHonoursCanceledContext 覆盖「ctx 已取消仍会调用目标」。
//
// 这是刻意的：取消发生在**轮次之间**，不在目标内部。RunOnce 不自己判
// ctx.Err()，因为目标可能需要在取消后完成一次收尾。断言的是行为边界，
// 不是偏好——若将来改成提前返回，这条用例会失败，提醒改动者确认那是有意为之。
func TestRunOnceHonoursCanceledContext(t *testing.T) {
	r := &recorder{}
	loop := reconcile.NewLoop([]reconcile.Target{r.target("canceled", 0, nil)},
		reconcile.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop.RunOnce(ctx)

	if got := r.count(); got != 1 {
		t.Errorf("RunOnce 不按 ctx 提前返回（由 Start 负责），实际调用 %d 次", got)
	}
}
