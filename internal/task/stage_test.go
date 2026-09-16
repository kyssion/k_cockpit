package task_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// errFake 模拟控制面自身的失败（节点做完了，后续步骤出错）。
var errFake = errors.New("测试失败")

// stageExecutor 是一个会上报阶段的执行器。
//
// 它经 agent.MockClient 走**真实的上报路径**，而不是自己直接调记录器：
// 阶段是由节点通过 Operation.OnStage 报上来的，绕开这一步就只测到了
// 「记录器能写库」，而真正容易接错的那一段（回调 → 记录器）没被覆盖。
type stageExecutor struct {
	client agent.Client
	kind   agent.OpKind
	fail   bool
}

func (e *stageExecutor) Type() string { return "test.stage" }

func (e *stageExecutor) Run(ctx context.Context, _ *model.Task) error {
	if _, err := task.ReporterFrom(ctx).Dispatch(ctx, e.client, agent.Operation{
		Kind:   e.kind,
		NodeID: 1,
		Target: "vm-1",
	}); err != nil {
		return err
	}
	if e.fail {
		return errFake
	}
	return nil
}

func stageTestEnv(t *testing.T, exec *stageExecutor) *task.Queue {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "stage.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.Task{}, &model.TaskStage{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	q := task.NewQueue(db, audit.NewRecorder(db), task.Options{
		MaxConcurrent: 1,
		PollInterval:  20 * time.Millisecond,
	})
	q.Register(exec)

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	t.Cleanup(func() {
		cancel()
		q.Stop()
	})

	return q
}

// TestStagesRecordedInOrder 覆盖「节点上报 → 记录器 → 落库 → 查询」这条链路。
func TestStagesRecordedInOrder(t *testing.T) {
	exec := &stageExecutor{client: agent.NewMockClient(), kind: agent.OpVMCreate}
	q := stageTestEnv(t, exec)
	ctx := context.Background()

	tk := enqueueStageTask(t, q, exec.Type())
	waitForTask(t, q, tk.ID, model.TaskSuccess)

	stages, err := q.Stages(ctx, tk.ID)
	if err != nil {
		t.Fatalf("查询阶段失败: %v", err)
	}

	// 期望：1 个控制面阶段（下发）+ 节点上报的 4 个步骤。
	//
	// 「下发指令」这一步必须存在：它是指令未送达时**唯一**能留下的痕迹——
	// 那种情况下节点什么都没做、也就什么都没上报。
	if len(stages) != 5 {
		t.Fatalf("阶段数 = %d, 期望 5: %v", len(stages), summarize(stages))
	}

	wantKeys := []string{
		"local.dispatch", "resource_check", "disk_allocate", "domain_define", "domain_start",
	}
	for i, want := range wantKeys {
		if stages[i].Key != want {
			t.Errorf("阶段[%d] = %q, 期望 %q", i, stages[i].Key, want)
		}
	}

	for i, s := range stages {
		// 序号必须严格递增且从 1 开始：时间线的顺序完全依赖它。时间戳在同
		// 一毫秒内会重复，靠时间排序得不到确定的顺序。
		if s.Seq != i+1 {
			t.Errorf("阶段[%d] 序号 = %d, 期望 %d", i, s.Seq, i+1)
		}
		if s.Status != model.StageSuccess {
			t.Errorf("阶段 %s 状态 = %q, 期望 success", s.Key, s.Status)
		}
		// 每个阶段都必须被收尾：停在 running 会让界面一直显示「进行中」，
		// 而任务其实早就结束了。
		if s.StartedAt == nil || s.FinishedAt == nil {
			t.Errorf("阶段 %s 缺少起止时间", s.Key)
			continue
		}
		if s.FinishedAt.Before(*s.StartedAt) {
			t.Errorf("阶段 %s 的结束时间早于开始时间", s.Key)
		}
	}
}

// TestStageProgressReachesHundred 覆盖进度的派生方式。
//
// 进度是阶段推进的**派生值**：结束时必须是 100，而不是「4 步都完成了但
// 进度停在 75」——那会让用户以为还有一步没走。
func TestStageProgressReachesHundred(t *testing.T) {
	exec := &stageExecutor{client: agent.NewMockClient(), kind: agent.OpVMCreate}
	q := stageTestEnv(t, exec)
	ctx := context.Background()

	tk := enqueueStageTask(t, q, exec.Type())
	waitForTask(t, q, tk.ID, model.TaskSuccess)

	got, err := q.Get(ctx, tk.ID, task.Viewer{UserID: 1})
	if err != nil {
		t.Fatalf("查询任务失败: %v", err)
	}
	if got.Progress != 100 {
		t.Errorf("完成后的进度 = %d, 期望 100", got.Progress)
	}
	if got.CurrentStage == nil || *got.CurrentStage != "domain_start" {
		t.Errorf("当前阶段 = %v, 期望 domain_start", got.CurrentStage)
	}
}

// TestFailedTaskClosesOpenStage 覆盖失败时阶段必须被收尾。
//
// 不收尾的后果不是「少一条信息」，而是**指向错误的方向**：阶段停在「执行中」
// 会让排查的人以为节点没响应，而真实原因其实写在任务上。
func TestFailedTaskClosesOpenStage(t *testing.T) {
	exec := &stageExecutor{client: agent.NewMockClient(), kind: agent.OpVMCreate, fail: true}
	q := stageTestEnv(t, exec)
	ctx := context.Background()

	tk := enqueueStageTask(t, q, exec.Type())
	waitForTask(t, q, tk.ID, model.TaskFailed)

	stages, err := q.Stages(ctx, tk.ID)
	if err != nil {
		t.Fatalf("查询阶段失败: %v", err)
	}
	if len(stages) == 0 {
		t.Fatal("失败的任务没有任何阶段记录")
	}

	last := stages[len(stages)-1]
	if last.Status == model.StageRunning || last.Status == model.StagePending {
		t.Errorf("最后一步 %s 未被收尾（状态 %q）", last.Key, last.Status)
	}
	// 失败原因要落在**阶段**上：任务只说「失败了」，阶段才回答「卡在哪」。
	if last.Status != model.StageFailed {
		t.Errorf("最后一步 %s 状态 = %q, 期望 failed", last.Key, last.Status)
	}
	if last.Message == nil || *last.Message == "" {
		t.Errorf("最后一步 %s 没有记录失败原因", last.Key)
	}
}

// TestUnregisteredOperationsReportNoStages 确认未登记的操作不会凭空多出阶段。
//
// 编几个通用步骤（「执行中 → 完成」）会让时间线看起来是齐的，却提供不了
// 任何定位能力——那比没有更糟，因为它让人以为阶段是齐全的。
func TestUnregisteredOperationsReportNoStages(t *testing.T) {
	var reported []string

	// 经 mock 走真实回调路径：断言的是**行为**（这个操作不上报阶段），
	// 而不是某个内部表里的条目。
	if _, err := agent.NewMockClient().Execute(context.Background(), agent.Operation{
		Kind:   agent.OpVMStatus,
		NodeID: 1,
		OnStage: func(s agent.Stage) {
			reported = append(reported, s.Key)
		},
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if len(reported) != 0 {
		t.Errorf("探测类操作上报了阶段 %v, 期望 0 个", reported)
	}
}

// --- 辅助 ---

func enqueueStageTask(t *testing.T, q *task.Queue, typ string) *model.Task {
	t.Helper()
	tk, err := q.Enqueue(context.Background(), task.Spec{
		Type:         typ,
		NodeID:       1,
		ResourceType: "vm",
		ResourceID:   1,
		ResourceName: "vm-1",
		OwnerID:      1,
		CreatedBy:    1,
	})
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	return tk
}

func waitForTask(t *testing.T, q *task.Queue, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := q.Get(context.Background(), id, task.Viewer{UserID: 1})
		if err == nil && got.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待任务 %d 进入 %s 超时", id, want)
}

func summarize(stages []model.TaskStage) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.Key+"/"+s.Status)
	}
	return out
}
