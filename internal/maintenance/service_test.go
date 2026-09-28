package maintenance_test

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/maintenance"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
)

func newTestService(t *testing.T) (*maintenance.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "maint.db"),
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
	if err := db.AutoMigrate(&model.Node{}, &model.SiteMaintenance{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	nodes := node.NewService(db, mockSnapshot{}, audit.NewRecorder(db), nil)
	return maintenance.NewService(db, nodes, audit.NewRecorder(db)), db
}

// mockSnapshot 让节点全部在线。
type mockSnapshot struct{}

func (mockSnapshot) Snapshot(_ context.Context, _ int64) (*agent.Snapshot, error) {
	return &agent.Snapshot{Status: agent.StatusOnline}, nil
}

func enrolledNode(t *testing.T, db *gorm.DB, name string, maintenance bool) int64 {
	t.Helper()
	agentID := "agent-" + name
	n := model.Node{
		Name: name, AgentID: &agentID, EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOnline, MaintenanceMode: maintenance,
	}
	if err := db.Create(&n).Error; err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	return n.ID
}

var adminView = authz.Viewer{UserID: 1, IsAdmin: true}

// Enter 应逐节点接管：手工维护的节点被跳过，其余进入维护。
func TestEnterTakesOverNodes(t *testing.T) {
	svc, db := newTestService(t)
	id1 := enrolledNode(t, db, "n1", false)
	id2 := enrolledNode(t, db, "n2", true) // 管理员手工设置的维护

	view, err := svc.Enter(context.Background(), maintenance.EnterRequest{Reason: "升级"},
		adminView, "admin", "127.0.0.1")
	if err != nil {
		t.Fatalf("进入维护失败: %v", err)
	}

	var n1, n2 model.Node
	_ = db.First(&n1, id1).Error
	_ = db.First(&n2, id2).Error
	if !n1.MaintenanceMode {
		t.Error("未手工维护的节点应被接管")
	}
	if !n2.MaintenanceMode {
		t.Error("手工维护的节点应保持维护（不应被退出）")
	}

	statuses := map[string]string{}
	for _, n := range view.Nodes {
		statuses[n.NodeName] = n.Status
	}
	if statuses["n1"] != "maintained" || statuses["n2"] != "skipped_manual" {
		t.Errorf("逐节点结果不符合预期: %+v", view.Nodes)
	}

	// 接管清单里只有 n1：退出时只应解除它。
	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("查询状态失败: %v", err)
	}
	if !st.InMaintenance || len(st.ManagedNodes) != 1 || st.ManagedNodes[0] != "n1" {
		t.Errorf("状态不符: %+v", st)
	}
}

// Exit 只解除本次接管的节点，手工维护的保持原状。
func TestExitKeepsManualMaintenance(t *testing.T) {
	svc, db := newTestService(t)
	id1 := enrolledNode(t, db, "n1", false)
	id2 := enrolledNode(t, db, "n2", true)

	if _, err := svc.Enter(context.Background(), maintenance.EnterRequest{Reason: "升级"},
		adminView, "admin", "127.0.0.1"); err != nil {
		t.Fatalf("进入维护失败: %v", err)
	}
	if _, err := svc.Exit(context.Background(), adminView, "admin", "127.0.0.1"); err != nil {
		t.Fatalf("退出维护失败: %v", err)
	}

	var n1, n2 model.Node
	_ = db.First(&n1, id1).Error
	_ = db.First(&n2, id2).Error
	if n1.MaintenanceMode {
		t.Error("接管的节点应已解除维护")
	}
	if !n2.MaintenanceMode {
		t.Error("手工维护的节点不应被退出操作解除")
	}

	st, _ := svc.Status(context.Background())
	if st.InMaintenance {
		t.Error("退出后站点不应再处于维护状态")
	}
}

// 幂等与冲突：重复进入、未进入就退出都是明确的业务错误。
func TestEnterExitConflicts(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.Exit(ctx, adminView, "admin", "127.0.0.1"); err == nil {
		t.Error("未进入维护时退出应报错")
	}
	if _, err := svc.Enter(ctx, maintenance.EnterRequest{}, adminView, "admin", "127.0.0.1"); err == nil {
		t.Error("缺维护原因应报错")
	}
	if _, err := svc.Enter(ctx, maintenance.EnterRequest{Reason: "升级"}, adminView, "admin", "127.0.0.1"); err != nil {
		t.Fatalf("首次进入失败: %v", err)
	}
	if _, err := svc.Enter(ctx, maintenance.EnterRequest{Reason: "再进一次"}, adminView, "admin", "127.0.0.1"); err == nil {
		t.Error("重复进入应报错")
	}
}
