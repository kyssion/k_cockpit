package task

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
)

func recoveryQueue(t *testing.T) (*Queue, *gorm.DB) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "recovery.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.Task{}, &model.TaskStage{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return NewQueue(db, nil, Options{MaxConcurrent: 1, PollInterval: time.Hour}), db
}

func TestRecoverStaleMarksUnknownThenFails(t *testing.T) {
	q, db := recoveryQueue(t)
	ctx := context.Background()

	row := model.Task{
		Type: "vm.create", Status: model.TaskRunning,
		CreatedAt: time.Now().Add(-time.Hour),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}

	q.recoverStale(ctx)
	var got model.Task
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskUnknown {
		t.Fatalf("重启后 running 应转 unknown，实际 %q", got.Status)
	}
	if got.Error == nil || *got.Error == "" {
		t.Fatal("unknown 任务应带说明")
	}

	// 把最后上报时间推到宽限期之外，sweep 应落为 failed 并释放锁。
	old := time.Now().Add(-unknownGrace - time.Minute)
	if err := db.Model(&model.Task{}).Where("id = ?", row.ID).
		Update("last_reported_at", old).Error; err != nil {
		t.Fatal(err)
	}
	q.sweepUnknown(ctx)
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.TaskFailed {
		t.Fatalf("超时 unknown 应转 failed，实际 %q", got.Status)
	}
	if got.FinishedAt == nil {
		t.Fatal("失败后必须写 finished_at")
	}
}
