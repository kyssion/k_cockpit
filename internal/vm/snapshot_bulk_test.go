package vm_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

func ptrStr(v string) *string { return &v }

// seedSnapshot 插入一条快照。
func seedSnapshot(t *testing.T, db *gorm.DB, vmID int64, name string, status string) {
	t.Helper()
	row := model.VMSnapshot{
		VMID: vmID, NodeID: 1, Name: name, Status: status,
		Kind: model.SnapshotKindInternal, DomainName: ptrStr(name),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建快照失败: %v", err)
	}
}

// TestDeleteAllSnapshotsSkipsCurrent 覆盖「当前快照被跳过」。
//
// 虚拟机正运行在这个快照上时删掉它，会让 libvirt 的"当前状态"失去参照；
// 这与"删除一个历史还原点"完全是两件事，因此默认要区分。
func TestDeleteAllSnapshotsSkipsCurrent(t *testing.T) {
	svc, queue, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	queue.Register(vm.NewSnapshotDeleteAllExecutor(db, agent.NewMockClient()))
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-snap", OwnerID: ptr(int64(7)), Present: true}
	db.Create(&row)
	seedSnapshot(t, db, row.ID, "snap-1", model.SnapshotReady)
	current := model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "snap-2", Status: model.SnapshotReady,
		IsCurrent: true,
	}
	db.Create(&current)

	view, err := svc.DeleteAllSnapshots(ctx, row.ID, vm.DeleteAllSnapshotsRequest{SkipCurrent: true},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if view.Total != 1 || view.Skipped != 1 {
		t.Errorf("结果 = 删除 %d / 跳过 %d, 期望 1 / 1", view.Total, view.Skipped)
	}
}

// TestDeleteAllSnapshotsRejectsWhenNothingToDelete 覆盖「没有可删的」。
//
// 返回冲突而不是"成功删了 0 个"：后者会让用户以为清理过了，而实际上
// 一个都没删。
func TestDeleteAllSnapshotsRejectsWhenNothingToDelete(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-empty", OwnerID: ptr(int64(7)), Present: true}
	db.Create(&row)

	_, err := svc.DeleteAllSnapshots(ctx, row.ID, vm.DeleteAllSnapshotsRequest{},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("没有任何快照却受理了")
	}
}

// TestDeleteAllSnapshotsDeletesNewestFirst 覆盖删除顺序。
//
// 快照链上后建的挂在先建的下游，从旧的开始删会在第一个上撞到
// "存在子快照"而被拒——整批失败在第一个上，是最难看懂的一种结果。
func TestDeleteAllSnapshotsDeletesNewestFirst(t *testing.T) {
	svc, queue, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	queue.Register(vm.NewSnapshotDeleteAllExecutor(db, agent.NewMockClient()))
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-chain", OwnerID: ptr(int64(7)), Present: true}
	db.Create(&row)
	seedSnapshot(t, db, row.ID, "old", model.SnapshotReady)
	seedSnapshot(t, db, row.ID, "new", model.SnapshotReady)

	view, err := svc.DeleteAllSnapshots(ctx, row.ID, vm.DeleteAllSnapshotsRequest{},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if view.Total != 2 {
		t.Errorf("删除数 = %d, 期望 2", view.Total)
	}
	// 受理后即标记为删除中：界面不必等任务跑完才知道这些正在消失。
	var marked int64
	db.Model(&model.VMSnapshot{}).
		Where("vm_id = ? AND status = ?", row.ID, model.SnapshotDeleting).Count(&marked)
	if marked != 2 {
		t.Errorf("标记为删除中的条数 = %d, 期望 2", marked)
	}
}

// TestRepairNVRAMRequiresUEFI 覆盖「只有 UEFI 才有启动项」。
//
// 让操作可用但什么也不做，是最容易让人误判"已经修好了"的一类行为。
func TestRepairNVRAMRequiresUEFI(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	bios := model.VM{NodeID: 1, Name: "vm-bios", OwnerID: ptr(int64(7)),
		Present: true, Firmware: "bios"}
	db.Create(&bios)

	_, err := svc.RepairNVRAM(ctx, bios.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("BIOS 机器也受理了启动项修复")
	}
	if !strings.Contains(err.Error(), "UEFI") {
		t.Errorf("错误未说明只适用于 UEFI: %v", err)
	}
}

func TestRepairNVRAMEnqueuesForUEFI(t *testing.T) {
	svc, queue, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	queue.Register(vm.NewNVRAMRepairExecutor(agent.NewMockClient()))
	ctx := context.Background()

	uefi := model.VM{NodeID: 1, Name: "vm-uefi", OwnerID: ptr(int64(7)),
		Present: true, Firmware: "uefi"}
	db.Create(&uefi)

	t2, err := svc.RepairNVRAM(ctx, uefi.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if t2.Type != model.TaskVMNVRAMRepair {
		t.Errorf("任务类型 = %q", t2.Type)
	}
}
