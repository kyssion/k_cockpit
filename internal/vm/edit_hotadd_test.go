package vm_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestEditFormReportsHotAddition 覆盖热添加信息的下发（G-33）。
//
// 前端按 hot_addition 解除 vCPU / 内存的禁用：矩阵里这两项标着
// requires_shutdown，没有这份信息的话，打开热添加的机器在运行中
// 也改不了——开关就成了摆设。
func TestEditFormReportsHotAddition(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	// 预置一台运行中、未开热添加的机器。
	row := &model.VM{
		NodeID: 1, Name: "no-hotplug", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("预置虚拟机失败: %v", err)
	}

	form, err := svc.EditFormOf(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取编辑表单失败: %v", err)
	}
	if form.EditableNow {
		t.Error("运行中的机器 EditableNow 应为 false")
	}
	if form.HotAddition["vcpu"] || form.HotAddition["memory_mb"] {
		t.Error("未开启热添加时不应有可热改项")
	}

	// 打开两个热添加开关后，两个键都应可热改。
	if err := db.Model(row).Updates(map[string]any{
		"cpu_hotplug": true, "memory_hotplug": true,
	}).Error; err != nil {
		t.Fatalf("更新热添加开关失败: %v", err)
	}
	form, err = svc.EditFormOf(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取编辑表单失败: %v", err)
	}
	if !form.HotAddition["vcpu"] || !form.HotAddition["memory_mb"] {
		t.Errorf("热添加信息 = %+v, 期望 vcpu 与 memory_mb 均可热改", form.HotAddition)
	}
}

// TestUpdateConfigHotAdditionRules 覆盖热改的受理校验（G-33）：
// 开关未开拒绝；只能增加；混合提交里含需关机项时仍拒绝。
func TestUpdateConfigHotAdditionRules(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := &model.VM{
		NodeID: 1, Name: "hotplug-vm", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		CPUHotplug: true,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("预置虚拟机失败: %v", err)
	}
	viewer := authz.Viewer{UserID: 7}

	// 开关未开：内存热改拒绝，错误要说清"为什么改不了"。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"memory_mb": 4096},
	}, viewer, "alice", "10.0.0.1")
	assertValidation(t, err, "内存热添加")

	// 只能增加：CPU 改小拒绝。
	_, err = svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"vcpu": 1},
	}, viewer, "alice", "10.0.0.1")
	assertValidation(t, err, "只能增加")

	// 开关已开 + 只增：CPU 热改受理。
	if _, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"vcpu": 4},
	}, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("CPU 热添加被误拒: %v", err)
	}

	// 混合提交：热改项合法，但固件仍需关机——整体拒绝。
	_, err = svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"vcpu": 4, "firmware": "uefi"},
	}, viewer, "alice", "10.0.0.1")
	assertValidation(t, err, "关机")
}

// assertValidation 断言 err 是带指定文案片段的业务校验错误。
func assertValidation(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（含 %q），实际通过", want)
	}
	apiErr, ok := err.(*api.Error)
	if !ok {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if !strings.Contains(apiErr.Message, want) {
		t.Fatalf("错误文案 %q 未包含 %q", apiErr.Message, want)
	}
}
