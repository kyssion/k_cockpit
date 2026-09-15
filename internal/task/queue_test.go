package task_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// fakeExecutor 是可控制结果与耗时的执行器。
type fakeExecutor struct {
	kind    string
	err     error
	delay   time.Duration
	started chan int64
}

func (f *fakeExecutor) Type() string { return f.kind }

func (f *fakeExecutor) Run(ctx context.Context, t *model.Task) error {
	if f.started != nil {
		f.started <- t.ID
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func newTestQueue(t *testing.T, executors ...task.Executor) (*task.Queue, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "task.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.Task{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	q := task.NewQueue(db, audit.NewRecorder(db), task.Options{
		MaxConcurrent: 4,
		PollInterval:  20 * time.Millisecond,
	})
	for _, e := range executors {
		q.Register(e)
	}
	return q, db
}

func startQueue(t *testing.T, q *task.Queue) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	t.Cleanup(func() {
		cancel()
		q.Stop()
	})
	return ctx
}

// waitFor 轮询等待条件成立。调度是异步的，测试不能依赖固定 sleep。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func assertAPIError(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != status {
		t.Errorf("状态码 = %d, 期望 %d (%s)", apiErr.Status, status, apiErr.Message)
	}
}

func TestEnqueueRejectsUnknownType(t *testing.T) {
	q, _ := newTestQueue(t)

	_, err := q.Enqueue(context.Background(), task.Spec{Type: "nope"})
	assertAPIError(t, err, 400)
}

func TestEnqueueAndExecute(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start"}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	created, err := q.Enqueue(ctx, task.Spec{
		Type:         "vm.start",
		ResourceType: "vm",
		ResourceID:   7,
		ResourceName: "vm-1",
		OwnerID:      3,
		CreatedBy:    3,
		Params:       map[string]any{"action": "start"},
	})
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if created.Status != model.TaskPending {
		t.Errorf("初始状态 = %q, 期望 pending", created.Status)
	}

	waitFor(t, "任务完成", func() bool {
		var got model.Task
		db.First(&got, created.ID)
		return got.Status == model.TaskSuccess
	})

	var got model.Task
	db.First(&got, created.ID)
	if got.Progress != 100 {
		t.Errorf("进度 = %d, 期望 100", got.Progress)
	}
	if got.StartedAt == nil || got.FinishedAt == nil || got.DispatchedAt == nil {
		t.Error("时间字段未完整记录")
	}
}

// 执行失败时，任务的 error 必须可展示且不含内部细节（R-009）。
func TestExecutionFailureIsSanitized(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", err: errors.New(`pq: relation "vm" does not exist`)}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	created, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 1, OwnerID: 1})
	waitFor(t, "任务失败", func() bool {
		var got model.Task
		db.First(&got, created.ID)
		return got.Status == model.TaskFailed
	})

	var got model.Task
	db.First(&got, created.ID)
	if got.Error == nil {
		t.Fatal("失败任务未记录原因")
	}
	for _, leak := range []string{"pq:", "relation", "does not exist"} {
		if strings.Contains(*got.Error, leak) {
			t.Errorf("错误信息泄漏内部细节 %q: %s", leak, *got.Error)
		}
	}
}

// 业务错误（*api.Error）的文案是面向用户的，应当原样保留。
func TestExecutionFailureKeepsBusinessMessage(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", err: api.Conflict("虚拟机已在运行中")}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	created, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 1, OwnerID: 1})
	waitFor(t, "任务失败", func() bool {
		var got model.Task
		db.First(&got, created.ID)
		return got.Status == model.TaskFailed
	})

	var got model.Task
	db.First(&got, created.ID)
	if got.Error == nil || *got.Error != "虚拟机已在运行中" {
		t.Errorf("业务错误文案被改写: %v", got.Error)
	}
}

// 幂等键重复时返回已有任务，而不是报错或新建。
func TestEnqueueIsIdempotent(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", delay: 200 * time.Millisecond}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	spec := task.Spec{
		Type: "vm.start", ResourceType: "vm", ResourceID: 1, OwnerID: 1,
		IdempotencyKey: "same-intent",
	}
	first, _ := q.Enqueue(ctx, spec)
	second, _ := q.Enqueue(ctx, spec)

	if first.ID != second.ID {
		t.Errorf("同一幂等键产生了不同任务: %d vs %d", first.ID, second.ID)
	}

	var count int64
	db.Model(&model.Task{}).Count(&count)
	if count != 1 {
		t.Errorf("任务数 = %d, 期望 1", count)
	}
}

// 同一资源的操作互斥：后到的任务保持 pending（R-003）。
func TestSameResourceIsSerialized(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", delay: 300 * time.Millisecond}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	first, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 42, OwnerID: 1})
	waitFor(t, "首个任务进入执行", func() bool {
		var got model.Task
		db.First(&got, first.ID)
		return got.Status == model.TaskRunning
	})

	second, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 42, OwnerID: 1})
	time.Sleep(100 * time.Millisecond)

	var got model.Task
	db.First(&got, second.ID)
	if got.Status != model.TaskPending {
		t.Errorf("同资源的第二个任务状态 = %q, 期望仍为 pending（应排队）", got.Status)
	}

	// 前一个完成后，后者应能执行。
	waitFor(t, "两个任务均完成", func() bool {
		var a, b model.Task
		db.First(&a, first.ID)
		db.First(&b, second.ID)
		return a.Status == model.TaskSuccess && b.Status == model.TaskSuccess
	})
}

func TestCancelPending(t *testing.T) {
	// 用慢执行器占住并发位，让后续任务保持 pending 以便测试取消。
	blocker := &fakeExecutor{kind: "blocker", delay: 500 * time.Millisecond}
	exec := &fakeExecutor{kind: "vm.start"}
	q, db := newTestQueue(t, blocker, exec)
	ctx := startQueue(t, q)

	// 占满并发（MaxConcurrent = 4）。
	for i := 0; i < 4; i++ {
		_, _ = q.Enqueue(ctx, task.Spec{Type: "blocker", ResourceType: "x", ResourceID: int64(i), OwnerID: 1})
	}
	time.Sleep(100 * time.Millisecond)

	target, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 9, OwnerID: 1})

	viewer := task.Viewer{UserID: 1}
	canceled, err := q.Cancel(ctx, target.ID, viewer, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if canceled.Status != model.TaskCanceled {
		t.Errorf("状态 = %q, 期望 canceled", canceled.Status)
	}
	if canceled.FinishedAt == nil {
		t.Error("取消后未记录结束时间")
	}

	var got model.Task
	db.First(&got, target.ID)
	if got.Status != model.TaskCanceled {
		t.Errorf("库中状态 = %q, 期望 canceled", got.Status)
	}
}

// 执行中的任务只置「取消请求」标记，不立刻判定为已取消（R-007）。
func TestCancelRunningMarksRequestOnly(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", delay: 400 * time.Millisecond}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	created, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 5, OwnerID: 1})
	waitFor(t, "进入执行", func() bool {
		var got model.Task
		db.First(&got, created.ID)
		return got.Status == model.TaskRunning
	})

	viewer := task.Viewer{UserID: 1}
	after, err := q.Cancel(ctx, created.ID, viewer, 1, "admin", "")
	if err != nil {
		t.Fatalf("取消请求失败: %v", err)
	}
	// 关键：此刻不能谎报 canceled——操作可能已经产生了副作用。
	if after.Status != model.TaskRunning {
		t.Errorf("状态 = %q, 期望仍为 running（等待执行方确认）", after.Status)
	}
	if !after.CancelRequested {
		t.Error("未记录取消请求")
	}

	// 执行结束后应落定为 canceled 而非 success。
	waitFor(t, "任务落定为已取消", func() bool {
		var got model.Task
		db.First(&got, created.ID)
		return got.Status == model.TaskCanceled
	})
}

func TestCancelTerminalRejected(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start"}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	created, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 1, OwnerID: 1})
	waitFor(t, "任务完成", func() bool {
		var got model.Task
		db.First(&got, created.ID)
		return got.Status == model.TaskSuccess
	})

	_, err := q.Cancel(ctx, created.ID, task.Viewer{UserID: 1}, 1, "admin", "")
	assertAPIError(t, err, 409)
}

// tenant 只能看到自己的任务；越权访问他人任务返回 404（不是 403）。
func TestOwnershipFiltering(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", delay: 50 * time.Millisecond}
	q, _ := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	mine, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 1, OwnerID: 10})
	theirs, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 2, OwnerID: 20})

	tenant := task.Viewer{UserID: 10}

	list, total, err := q.List(ctx, task.Filter{Viewer: tenant})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].ID != mine.ID {
		t.Errorf("tenant 看到了不该看到的任务: total=%d list=%v", total, list)
	}

	// 越权访问他人任务：404 而非 403（后者等于确认了该 ID 存在）。
	_, err = q.Get(ctx, theirs.ID, tenant)
	assertAPIError(t, err, 404)

	// 管理员可见全部。
	adminList, adminTotal, _ := q.List(ctx, task.Filter{Viewer: task.Viewer{UserID: 99, IsAdmin: true}})
	if adminTotal != 2 || len(adminList) != 2 {
		t.Errorf("管理员应看到全部任务: total=%d", adminTotal)
	}
}

// 清理只允许删除终态任务（R-016）。
func TestCleanupOnlyRemovesTerminalTasks(t *testing.T) {
	exec := &fakeExecutor{kind: "vm.start", delay: 300 * time.Millisecond}
	q, db := newTestQueue(t, exec)
	ctx := startQueue(t, q)

	done, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 1, OwnerID: 1})
	waitFor(t, "任务完成", func() bool {
		var got model.Task
		db.First(&got, done.ID)
		return got.Status == model.TaskSuccess
	})

	running, _ := q.Enqueue(ctx, task.Spec{Type: "vm.start", ResourceType: "vm", ResourceID: 2, OwnerID: 1})
	waitFor(t, "任务执行中", func() bool {
		var got model.Task
		db.First(&got, running.ID)
		return got.Status == model.TaskRunning
	})

	removed, err := q.Cleanup(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if removed != 1 {
		t.Errorf("清理数量 = %d, 期望 1（只清终态）", removed)
	}

	var stillThere model.Task
	if err := db.First(&stillThere, running.ID).Error; err != nil {
		t.Error("执行中的任务被清理了——删除它会让结果永远无处落定")
	}
}
