package vm_test

import (
	"context"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// TestUpdateConfigRejectsUnsupportedKey 守住"不假装生效"这条线：
// 节点侧没实现的配置项必须在受理阶段拒绝，不能入队后报成功。
func TestUpdateConfigRejectsUnsupportedKey(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-unsupported", OwnerID: ptr(int64(7))}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("准备虚拟机失败: %v", err)
	}

	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"firmware": "uefi"},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertValidation(t, err, "不支持在线修改")

	var count int64
	if err := db.Model(&model.Task{}).Count(&count).Error; err != nil {
		t.Fatalf("统计任务失败: %v", err)
	}
	if count != 0 {
		t.Errorf("被拒绝的配置不应产生任务，实际 %d 条", count)
	}
}

// TestUpdateConfigRejectsMetadataOnConfigEndpoint 防止把备注混进配置下发。
func TestUpdateConfigRejectsMetadataOnConfigEndpoint(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-metadata", OwnerID: ptr(int64(7))}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("准备虚拟机失败: %v", err)
	}

	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"remark": "x"},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertValidation(t, err, "元数据接口")
}

// TestEditFormMarksUnsupportedFields 确保界面能提前知道哪些项改不了。
func TestEditFormMarksUnsupportedFields(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-edit-marks", OwnerID: ptr(int64(7))}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("准备虚拟机失败: %v", err)
	}

	form, err := svc.EditFormOf(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("读取编辑表单失败: %v", err)
	}

	marks := map[string]bool{}
	for _, f := range form.Fields {
		marks[f.Key] = f.NotApplied
	}
	for key, want := range map[string]bool{
		"firmware":       true,  // 需要重建域
		"machine_type":   true,  // 需要重建域
		"nested_virt":    true,  // 尚无节点实现
		"vcpu":           false, // 已实现
		"boot_order":     false, // 已实现
		"cpu_affinity":   false, // 已实现
		"disk_iops_read": false, // 已实现
	} {
		got, ok := marks[key]
		if !ok {
			t.Errorf("矩阵里找不到字段 %q", key)
			continue
		}
		if got != want {
			t.Errorf("字段 %q 的 not_applied = %v, 期望 %v", key, got, want)
		}
	}
}
