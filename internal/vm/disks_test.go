package vm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestDisksListsNodeState 覆盖磁盘列表来自**节点探测**而非控制面记录。
//
// 控制面不持有磁盘：存一份就要与虚拟化层对账，而多一块少一块不会报错，
// 只会在某次操作时变成"改了一块不存在的盘"。
func TestDisksListsNodeState(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-disks", OwnerID: ptr(int64(7))}
	db.Create(&row)

	view, err := svc.Disks(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("读取磁盘列表失败: %v", err)
	}
	if len(view.Disks) != 2 {
		t.Fatalf("磁盘数 = %d, 期望 2（mock 给系统盘 + 数据盘）", len(view.Disks))
	}
	if view.Disks[0].Dev != "vda" || !view.Disks[0].IsSystem {
		t.Errorf("第一块盘 = %+v, 期望系统盘 vda", view.Disks[0])
	}
	// 可选值来自配置矩阵，而不是前端另写一份。
	if len(view.BusOptions) == 0 {
		t.Error("未下发总线可选值")
	}
}

// TestDisksMarksActionAvailability 覆盖「能不能操作」由控制面按运行态算。
//
// 节点只报告事实（是不是系统盘、能不能热插拔）；规则只有一份才不会分叉：
// 界面按它禁用按钮，后端按它拒绝请求。
func TestDisksMarksActionAvailability(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-running", OwnerID: ptr(int64(7))}
	db.Create(&row)

	view, err := svc.Disks(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("读取磁盘列表失败: %v", err)
	}
	byDev := map[string]vm.DiskView{}
	for _, d := range view.Disks {
		byDev[d.Dev] = d
	}
	if byDev["vda"].CanDetach {
		t.Error("系统盘被标记为可卸载")
	}
	// 运行中且支持热插拔的数据盘可以卸载。
	if !byDev["vdb"].CanDetach {
		t.Errorf("运行中可热插拔的 vdb 应可卸载: %s", byDev["vdb"].DetachReason)
	}
	// 换总线一律需要关机。
	if byDev["vdb"].CanChangeBus {
		t.Error("运行中不应允许换总线")
	}
	if byDev["vdb"].ChangeBusReason == "" {
		t.Error("换总线被禁用但没有给出原因")
	}
}

// TestChangeDiskRejectsBusChangeWhileRunning 覆盖运行态约束。
func TestChangeDiskRejectsBusChangeWhileRunning(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-bus", OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionBus, Dev: "vdb", Bus: "virtio",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("运行中的换总线未被拒绝")
	}
	if !strings.Contains(err.Error(), "关机") {
		t.Errorf("错误未说明需要关机: %v", err)
	}
}

// TestChangeDiskRejectsDetachingSystemDisk 覆盖系统盘保护。
//
// 「vda 一定是系统盘」这种猜测在机型不同的机器上会错，因此判定交给节点。
func TestChangeDiskRejectsDetachingSystemDisk(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-sys", OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionDetach, Dev: "vda",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("卸载系统盘未被拒绝")
	}
	if !strings.Contains(err.Error(), "系统盘") {
		t.Errorf("错误未说明是系统盘: %v", err)
	}
}

// TestChangeDiskRejectsFileOnAnotherNode 覆盖「磁盘文件必须在同一节点」。
func TestChangeDiskRejectsFileOnAnotherNode(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-file", OwnerID: ptr(int64(7))}
	db.Create(&row)
	uploaded := time.Now()
	file := model.StorageFile{
		NodeID: 2, UserID: ptr(int64(7)), RelPath: "data.img",
		Category: model.FileCategoryDisk, Filename: "data.img", UploadedAt: &uploaded,
	}
	db.Create(&file)

	_, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionAttach, FileID: file.ID,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("跨节点挂载未被拒绝")
	}
	if !strings.Contains(err.Error(), "节点") {
		t.Errorf("错误未指出节点不匹配: %v", err)
	}
}

// TestDisksRejectsForeignVM 覆盖归属过滤。
func TestDisksRejectsForeignVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-foreign", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 用 404 而不是 403：后者会确认「这个 ID 存在」。
	_, err := svc.Disks(ctx, row.ID, authz.Viewer{UserID: 8})
	assertAPIError(t, err, 404)
}
