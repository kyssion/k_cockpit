package vm_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// seedTargetNodeNamed 建一个指定状态的已接入节点（名字唯一，避免撞唯一索引）。
func seedTargetNodeNamed(t *testing.T, db *gorm.DB, name string, online, maintenance bool) int64 {
	t.Helper()
	status := model.NodeStatusOffline
	if online {
		status = model.NodeStatusOnline
	}
	row := model.Node{
		Name: name, EnrollState: model.NodeEnrollEnrolled,
		Status: status, MaintenanceMode: maintenance,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建节点 %s 失败: %v", name, err)
	}
	return row.ID
}

// TestMigrateTargets 覆盖目标清单的聚合判定（F-6-03 / F-6-04）：
// 容量来自最近一次采样、维护与冲突分别给出不可用原因、建议目标只有一个。
func TestMigrateTargets(t *testing.T) {
	svc, _, db := newTestEnv(t)
	if err := db.AutoMigrate(&model.HostStatsRecord{}); err != nil {
		t.Fatalf("建采样表失败: %v", err)
	}
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-targets")

	good := seedTargetNodeNamed(t, db, "node-good", true, false)
	_ = seedTargetNodeNamed(t, db, "node-maint", true, true)
	conflicted := seedTargetNodeNamed(t, db, "node-conflict", true, false)

	// 好节点：有采样（8 核、总 16G 用 4G）+ 一个就绪池（200G 可用）。
	if err := db.Create(&model.HostStatsRecord{
		NodeID: good, At: time.Now(),
		CPUCores: 8, MemTotalMB: 16384, MemUsedMB: 4096,
	}).Error; err != nil {
		t.Fatalf("建采样失败: %v", err)
	}
	if err := db.Create(&model.StoragePool{
		NodeID: good, Status: model.StoragePoolReady, TotalBytes: 500 << 30, UsableBytes: 200 << 30,
	}).Error; err != nil {
		t.Fatalf("建存储池失败: %v", err)
	}

	// 冲突节点：本机持有的静态地址在那边已被别人占用。
	if err := db.Create(&model.StaticIP{
		NodeID: row.NodeID, VMID: &row.ID, IP: "192.168.1.50", AddressFamily: "ipv4",
	}).Error; err != nil {
		t.Fatalf("建本机静态地址失败: %v", err)
	}
	other := int64(999)
	if err := db.Create(&model.StaticIP{
		NodeID: conflicted, VMID: &other, IP: "192.168.1.50", AddressFamily: "ipv4",
	}).Error; err != nil {
		t.Fatalf("建冲突地址失败: %v", err)
	}

	items, err := svc.MigrateTargets(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询目标失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("应有 3 个目标节点, 实际 %d: %+v", len(items), items)
	}

	byName := map[string]vm.MigrateTargetView{}
	for _, it := range items {
		byName[it.NodeName] = it
	}

	g := byName["node-good"]
	if !g.Suitable || !g.Recommended {
		t.Errorf("node-good 应为可用且被推荐: %+v", g)
	}
	if !g.StatsKnown || g.CPUCores != 8 || g.MemFreeMB != 12288 || g.StorageFreeGB != 200 {
		t.Errorf("node-good 容量聚合不符: %+v", g)
	}

	m := byName["node-maint"]
	if m.Suitable || m.Reason == "" {
		t.Errorf("维护中的节点应为不可用且带原因: %+v", m)
	}

	c := byName["node-conflict"]
	if c.Suitable || len(c.Conflicts) == 0 {
		t.Errorf("地址冲突的节点应为不可用且列出冲突: %+v", c)
	}
}

// 归属校验：租户不能通过目标清单探到别人的虚拟机（404 而非 403）。
func TestMigrateTargetsRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-targets-theirs")

	_, err := svc.MigrateTargets(ctx, row.ID, authz.Viewer{UserID: 20})
	assertAPIError(t, err, 404)
}
