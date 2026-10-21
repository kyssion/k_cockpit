package task_test

import (
	"context"
	"path/filepath"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/ops/task"
)

// TestEnqueueReusesKeyOnlyInFlight 终态任务的幂等键必须可被新意图接管：
// 失败的创建不应把名字挡到保留期清理之后。
func TestEnqueueReusesKeyOnlyInFlight(t *testing.T) {
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite, Path: filepath.Join(t.TempDir(), "q.db"),
		MaxOpenConns: 1, MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.Task{}, &model.TaskStage{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表: %v", err)
	}
	q := task.NewQueue(db, audit.NewRecorder(db), task.Options{})
	t.Cleanup(q.Stop)
	// 注册一个空执行器让类型合法（队列在入队处校验执行器存在）。
	q.Register(noopExecutor{typ: "test.kind"})

	spec := task.Spec{
		Type: "test.kind", ResourceType: "vm", ResourceName: "vm1",
		IdempotencyKey: "test:key-1",
	}
	first, err := q.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatalf("首次入队: %v", err)
	}
	// 在途：同键返回同任务。
	again, err := q.Enqueue(context.Background(), spec)
	if err != nil || again.ID != first.ID {
		t.Fatalf("在途同键应返回原任务: %v %d vs %d", err, again.ID, first.ID)
	}
	// 置为终态失败。
	if err := db.Model(&model.Task{}).Where("id = ?", first.ID).
		Updates(map[string]any{"status": model.TaskFailed}).Error; err != nil {
		t.Fatalf("置失败: %v", err)
	}
	// 终态后同键：应得到**新**任务，而不是旧的失败任务。
	fresh, err := q.Enqueue(context.Background(), spec)
	if err != nil {
		t.Fatalf("终态后入队: %v", err)
	}
	if fresh.ID == first.ID {
		t.Fatalf("终态后同键不应返回旧任务（拿回失败的 %d）", first.ID)
	}
	if fresh.Status == model.TaskFailed {
		t.Fatalf("新任务不应继承失败状态: %s", fresh.Status)
	}
}

type noopExecutor struct{ typ string }

func (e noopExecutor) Type() string                               { return e.typ }
func (e noopExecutor) Run(_ context.Context, _ *model.Task) error { return nil }
