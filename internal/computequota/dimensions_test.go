package computequota_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/computequota"
	"k_cockpit/internal/model"
)

// seedSnapshot 为某台虚拟机插入一条快照。
func seedSnapshot(t *testing.T, db *gorm.DB, vmID int64, name string) {
	t.Helper()
	if err := db.Create(&model.VMSnapshot{
		VMID: vmID, NodeID: 1, Name: name, Status: model.SnapshotReady,
	}).Error; err != nil {
		t.Fatalf("创建快照失败: %v", err)
	}
}

// seedPortForward 插入一条端口转发。
func seedPortForward(t *testing.T, db *gorm.DB, vmID int64, port int) {
	t.Helper()
	if err := db.Create(&model.PortForward{
		NodeID: 1, VMID: &vmID, Protocol: model.PortProtocolTCP, HostPort: port, TargetPort: 22,
	}).Error; err != nil {
		t.Fatalf("创建端口转发失败: %v", err)
	}
}

// seedPublicIP 插入一条未释放的公网 IP 绑定。
func seedPublicIP(t *testing.T, db *gorm.DB, vmID int64) {
	t.Helper()
	if err := db.Create(&model.PublicIPBinding{
		PublicIPID: 1, NodeID: 1, VMID: &vmID, Mode: model.PublicIPModeLabel("nat_1to1"),
		RuntimeStatus: model.BindingPending,
	}).Error; err != nil {
		t.Fatalf("创建公网 IP 绑定失败: %v", err)
	}
}

// TestSnapshotQuotaRejectsExtraSnapshot 覆盖「快照上限按用户 × 节点计」。
//
// 按用户而不是按单台机器计，是这里唯一合理的口径：用户把机器删掉重建
// 就能绕过去的配额，等于没有配额。
func TestSnapshotQuotaRejectsExtraSnapshot(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedVM(t, db, "vm-1", 7, 2, 2048)

	var vm model.VM
	if err := db.Where("name = ?", "vm-1").First(&vm).Error; err != nil {
		t.Fatalf("读取虚拟机失败: %v", err)
	}

	if err := svc.Set(ctx, 1, 7, computequota.Limits{Snapshots: 1}, 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	seedSnapshot(t, db, vm.ID, "snap-1")

	if err := svc.Check(ctx, 7, 1, computequota.Additions{Snapshots: 1}); err == nil {
		t.Fatal("超出快照上限未被拒绝")
	} else if !strings.Contains(err.Error(), "快照数量") {
		t.Errorf("错误未指出是快照维度: %v", err)
	}
}

// TestPortForwardQuota 覆盖端口转发与公网 IP 两个维度。
//
// 它们此前**完全没有上限**：界面上写着配额，而这两类资源不受任何约束——
// 用户照着配额规划，撞上的却是另一套规则。
func TestPortForwardQuota(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedVM(t, db, "vm-1", 7, 2, 2048)
	var vm model.VM
	if err := db.Where("name = ?", "vm-1").First(&vm).Error; err != nil {
		t.Fatalf("读取虚拟机失败: %v", err)
	}

	if err := svc.Set(ctx, 1, 7, computequota.Limits{PortForwards: 1, PublicIPs: 1},
		1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	seedPortForward(t, db, vm.ID, 10022)
	seedPublicIP(t, db, vm.ID)

	if err := svc.Check(ctx, 7, 1, computequota.Additions{PortForwards: 1}); err == nil {
		t.Fatal("超出端口转发上限未被拒绝")
	}
	if err := svc.Check(ctx, 7, 1, computequota.Additions{PublicIPs: 1}); err == nil {
		t.Fatal("超出公网 IP 上限未被拒绝")
	}
	// 未触及的维度仍应放行：只加一个快照不该被转发上限拦住。
	if err := svc.Check(ctx, 7, 1, computequota.Additions{Snapshots: 1}); err != nil {
		t.Errorf("未设限的维度被拒绝: %v", err)
	}
}

// TestListReportsNewDimensions 覆盖列表接口把数量型维度一并返回：
// 管理员要在一个页面里看到"这个用户占了多少"，而不是再打开三个页面。
func TestListReportsNewDimensions(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedVM(t, db, "vm-1", 7, 2, 2048)
	var vm model.VM
	if err := db.Where("name = ?", "vm-1").First(&vm).Error; err != nil {
		t.Fatalf("读取虚拟机失败: %v", err)
	}
	seedSnapshot(t, db, vm.ID, "snap-1")
	seedPortForward(t, db, vm.ID, 10022)

	if err := svc.Set(ctx, 1, 7, computequota.Limits{Snapshots: 5, PortForwards: 3},
		1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	items, err := svc.List(ctx, 1)
	if err != nil {
		t.Fatalf("列出配额失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("条目数 = %d, 期望 1", len(items))
	}
	it := items[0]
	if it.Snapshots != 1 || it.PortForwards != 1 {
		t.Errorf("占用 = 快照 %d / 转发 %d, 期望各 1", it.Snapshots, it.PortForwards)
	}
	if it.QuotaSnapshots != 5 || it.QuotaPortForwards != 3 {
		t.Errorf("上限 = 快照 %d / 转发 %d, 期望 5 / 3", it.QuotaSnapshots, it.QuotaPortForwards)
	}
}

// TestSnapshotLimitFallsBackToDefault 覆盖"没配配额就退回默认值"。
//
// 新增维度不该在一次升级里悄悄改变既有系统的边界——没配过的地方，行为
// 必须与从前完全一致。
func TestSnapshotLimitFallsBackToDefault(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	n, err := svc.SnapshotLimit(ctx, 7, 1)
	if err != nil {
		t.Fatalf("读取快照上限失败: %v", err)
	}
	if n != 0 {
		t.Errorf("未配额时快照上限 = %d, 期望 0（由调用方退回默认值）", n)
	}
}
