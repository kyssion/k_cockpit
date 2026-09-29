package vm_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/compute/vm"
	"k_cockpit/internal/model"
)

// 自动弹出循环（F-2-17）：初始化就绪后清标记、弹介质、入队下发。
func TestMediaEjectRunOnce(t *testing.T) {
	svc, queue, db := newTestEnv(t)
	ctx := context.Background()

	// 一台运行中、欠一次自动弹出的机器，光驱里插着安装 ISO。
	row := model.VM{
		NodeID: 1, Name: "vm-eject", OwnerID: ptr(int64(7)),
		Status: model.VMStatusRunning, Present: true, MediaAutoEject: true,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("建虚拟机失败: %v", err)
	}
	isoID := int64(11)
	cd := model.VMCDROM{VMID: row.ID, NodeID: 1, OrderNo: 0, ISOFileID: &isoID, Bus: "sata"}
	if err := db.Create(&cd).Error; err != nil {
		t.Fatalf("建光驱失败: %v", err)
	}

	loop := vm.NewMediaEjectLoop(svc, db, agent.NewMockClient(), nil, vm.MediaEjectOptions{})
	n, err := loop.RunOnce(ctx)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应弹出 1 台, 实际 %d", n)
	}

	var after model.VM
	_ = db.First(&after, row.ID).Error
	if after.MediaAutoEject {
		t.Error("弹出后应清除标记，否则每分钟会重复弹")
	}
	var cdAfter model.VMCDROM
	_ = db.First(&cdAfter, cd.ID).Error
	if cdAfter.ISOFileID != nil {
		t.Error("光驱里的介质应已弹出（iso_file_id 置空）")
	}
	// 下发任务入队（光驱设备保留）。
	var tasks int64
	db.Model(&model.Task{}).Where("resource_id = ? AND type = ?", row.ID, model.TaskVMCDROMApply).Count(&tasks)
	if tasks != 1 {
		t.Errorf("应入队 1 个光驱下发任务, 实际 %d", tasks)
	}
	_ = queue
}
