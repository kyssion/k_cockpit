package alert_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/alert"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

func newEnv(t *testing.T) (*gorm.DB, *alert.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "alert.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.Alert{}, &model.Node{}, &model.VM{}, &model.Task{},
		&model.ResourceQuota{}, &model.StoragePool{}, &model.HostStatsRecord{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db, alert.NewService(db)
}

// TestEvaluateCreatesAlertForOfflineNode 覆盖最基本的一条：离线节点要报警。
func TestEvaluateCreatesAlertForOfflineNode(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	hb := time.Now().Add(-2 * time.Minute)
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOffline, LastHeartbeatAt: &hb,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	changed, err := svc.Evaluate(ctx)
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if changed == 0 {
		t.Fatal("离线节点未产生告警")
	}

	items, err := svc.List(ctx, alert.ListOptions{})
	if err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	found := false
	for _, it := range items {
		if it.Kind == alert.KindNodeOffline {
			found = true
			if it.Level != model.AlertLevelDanger {
				t.Errorf("离线告警级别 = %s, 期望 danger", it.Level)
			}
		}
	}
	if !found {
		t.Errorf("未找到离线告警: %+v", items)
	}
}

// TestEvaluateIsIdempotent 覆盖去重：同一问题反复评估只应有一条。
//
// 这是告警与事件流的根本区别——评估每五分钟跑一次，没有去重的话用户看到
// 的是几百条重复。
func TestEvaluateIsIdempotent(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	hb := time.Now().Add(-2 * time.Minute)
	db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOffline, LastHeartbeatAt: &hb,
	})

	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("首次评估失败: %v", err)
	}
	changed, err := svc.Evaluate(ctx)
	if err != nil {
		t.Fatalf("二次评估失败: %v", err)
	}
	if changed != 0 {
		t.Errorf("重复评估产生了变化（应为幂等）: %d", changed)
	}

	var n int64
	db.Model(&model.Alert{}).Count(&n)
	if n != 1 {
		t.Errorf("告警条数 = %d, 期望 1", n)
	}
}

// TestEvaluateClearsWhenRecovered 覆盖自动关闭，以及**恢复后重新出现**。
func TestEvaluateClearsWhenRecovered(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	hb := time.Now().Add(-2 * time.Minute)
	node := model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOffline, LastHeartbeatAt: &hb,
	}
	db.Create(&node)

	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	// 节点恢复在线。
	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("status", model.NodeStatusOnline).Error; err != nil {
		t.Fatalf("更新节点失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	var row model.Alert
	if err := db.Where("kind = ?", alert.KindNodeOffline).First(&row).Error; err != nil {
		t.Fatalf("告警行应保留（关闭而非删除）: %v", err)
	}
	if row.Status != model.AlertCleared {
		t.Errorf("恢复后状态 = %s, 期望 cleared", row.Status)
	}

	// 再次离线：应重新变成 active，而不是继续躺在 cleared 里。
	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("status", model.NodeStatusOffline).Error; err != nil {
		t.Fatalf("更新节点失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if err := db.Where("kind = ?", alert.KindNodeOffline).First(&row).Error; err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	if row.Status != model.AlertActive {
		t.Errorf("重新出现后状态 = %s, 期望重新变为 active", row.Status)
	}
}

// TestAckKeepsAlertOpen 覆盖「确认 ≠ 解决」。
//
// 确认只是让它不再打扰人；把它做成"关掉"会让一个仍然存在的问题从视野里
// 消失，而没有任何地方记得它还在。
func TestAckKeepsAlertOpen(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	hb := time.Now().Add(-2 * time.Minute)
	db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOffline, LastHeartbeatAt: &hb,
	})
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	items, _ := svc.List(ctx, alert.ListOptions{})
	if err := svc.Ack(ctx, items[0].ID, authz.Viewer{UserID: 9}); err != nil {
		t.Fatalf("确认失败: %v", err)
	}

	active, _, err := svc.Counts(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if active != 0 {
		t.Errorf("确认后仍未确认数 = %d, 期望 0", active)
	}
	// 但告警本身**还在列表里**（只是状态变了）。
	after, _ := svc.List(ctx, alert.ListOptions{})
	if len(after) != 1 {
		t.Fatalf("确认后告警消失: %+v", after)
	}
	if after[0].Status != model.AlertAcked {
		t.Errorf("状态 = %s, 期望 acked", after[0].Status)
	}
}

// TestEvaluateReportsFailedTasksPerTask 覆盖失败任务按任务告警（便于定位）。
func TestEvaluateReportsFailedTasksPerTask(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	finished := time.Now().UTC().Add(-time.Hour)
	name := "vm-1"
	errMsg := "磁盘写入失败"
	db.Create(&model.Task{
		Type: model.TaskVMCreate, Status: model.TaskFailed,
		ResourceName: &name, Error: &errMsg, FinishedAt: &finished,
	})

	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	items, _ := svc.List(ctx, alert.ListOptions{Level: model.AlertLevelDanger})
	found := false
	for _, it := range items {
		if it.Kind == alert.KindTaskFailed {
			found = true
			if it.Detail == "" {
				t.Error("失败任务告警未带出原因——用户还要去任务中心再查一次")
			}
		}
	}
	if !found {
		t.Errorf("未产生失败任务告警: %+v", items)
	}
}
