package scheduler

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

func newRetentionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "retention.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.Task{}, &model.TaskStage{}, &model.AuditLog{},
		&model.NetworkCapture{}, &model.SchedulerEvent{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

func countOf(t *testing.T, db *gorm.DB, m any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(m).Count(&n).Error; err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	return n
}

// 清理必须只碰「终态且过期」的任务，运行中与结果未知的任务留着对账；
// 阶段要跟着任务一起消失，抓包记录按 expires_at 或 created_at 到期即删。
func TestRunDataRetention(t *testing.T) {
	db := newRetentionDB(t)
	ctx := context.Background()
	old := time.Now().Add(-40 * 24 * time.Hour)
	ancient := time.Now().Add(-400 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)

	tasks := []model.Task{
		{Type: "vm.create", Status: model.TaskSuccess, FinishedAt: &old, CreatedAt: old},
		{Type: "vm.create", Status: model.TaskSuccess, FinishedAt: &recent, CreatedAt: recent},
		{Type: "vm.create", Status: model.TaskRunning, CreatedAt: old},
		{Type: "vm.create", Status: model.TaskUnknown, CreatedAt: old},
	}
	if err := db.Create(&tasks).Error; err != nil {
		t.Fatalf("写入任务失败: %v", err)
	}
	if err := db.Create(&model.TaskStage{TaskID: tasks[0].ID, Seq: 1, Key: "prepare", Status: "done"}).Error; err != nil {
		t.Fatalf("写入阶段失败: %v", err)
	}

	audits := []model.AuditLog{
		{At: ancient, ResourceType: "vm", Action: "vm.create", Success: true},
		{At: recent, ResourceType: "vm", Action: "vm.create", Success: true},
	}
	if err := db.Create(&audits).Error; err != nil {
		t.Fatalf("写入审计失败: %v", err)
	}

	captures := []model.NetworkCapture{
		{NodeID: 1, ExpiresAt: &old, CreatedAt: old},
		{NodeID: 1, ExpiresAt: &future, CreatedAt: recent},
		{NodeID: 1, CreatedAt: old},
		{NodeID: 1, CreatedAt: recent},
	}
	if err := db.Create(&captures).Error; err != nil {
		t.Fatalf("写入抓包记录失败: %v", err)
	}

	loop := NewRetentionLoop(db, nil, RetentionOptions{})
	deleted, err := loop.RunDataRetention(ctx)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if deleted != 4 {
		t.Fatalf("删除条数 = %d，期望 4（1 任务 + 1 审计 + 2 抓包）", deleted)
	}
	if got := countOf(t, db, &model.Task{}); got != 3 {
		t.Errorf("任务应保留 3 条（近期终态 / running / unknown），实际 %d", got)
	}
	if got := countOf(t, db, &model.TaskStage{}); got != 0 {
		t.Errorf("过期任务的阶段应一并删除，实际剩 %d", got)
	}
	if got := countOf(t, db, &model.AuditLog{}); got != 1 {
		t.Errorf("审计日志应保留 1 条，实际 %d", got)
	}
	if got := countOf(t, db, &model.NetworkCapture{}); got != 2 {
		t.Errorf("抓包记录应保留 2 条，实际 %d", got)
	}
}
