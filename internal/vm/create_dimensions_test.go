package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestCreateFormPublishesNewDimensions 覆盖 G-29 的新维度随矩阵下发。
//
// 向导按矩阵渲染：矩阵里没有的字段，用户在界面上就设不了——因此「表单里
// 有没有」是这条链路的第一道断言。
func TestCreateFormPublishesNewDimensions(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	form, err := svc.CreateFormOf(ctx, 1, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取创建表单失败: %v", err)
	}
	byKey := map[string]vm.EditField{}
	for _, f := range form.Fields {
		byKey[f.Key] = f
	}
	for _, key := range []string{"video_model", "rtc_mode", "arch", "cpu_hotplug", "memory_hotplug"} {
		f, ok := byKey[key]
		if !ok {
			t.Errorf("创建表单缺少 G-29 字段 %s", key)
			continue
		}
		if f.Kind == vm.EditKindSelect && len(f.Options) == 0 {
			t.Errorf("%s 是枚举字段但没有可选值", key)
		}
	}
	// machine_type 需要带 virt 选项：aarch64 的唯一合法机型。
	foundVirt := false
	for _, o := range byKey["machine_type"].Options {
		if o.Value == "virt" {
			foundVirt = true
		}
	}
	if !foundVirt {
		t.Error("machine_type 缺少 virt 选项")
	}
}

// TestCreateRejectsInvalidCombinations 覆盖跨字段组合校验（G-29）。
//
// 每个值单独看都在矩阵的合法范围内，放在一起却引导不了——这类错误只靠
// 矩阵的逐项校验拦不住，而放过去的表现是虚拟机卡在固件画面，要到装系统
// 时才暴露。
func TestCreateRejectsInvalidCombinations(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	base := vm.CreateRequest{
		Name: "combo", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}

	cases := []struct {
		name    string
		mutate  func(*vm.CreateRequest)
		wantMsg string
	}{
		{
			name:    "aarch64 配 q35",
			mutate:  func(r *vm.CreateRequest) { r.Arch = "aarch64"; r.MachineType = "q35" },
			wantMsg: "virt",
		},
		{
			name:    "aarch64 配 BIOS",
			mutate:  func(r *vm.CreateRequest) { r.Arch = "aarch64"; r.MachineType = "virt"; r.Firmware = "bios" },
			wantMsg: "UEFI",
		},
		{
			name: "aarch64 配 x86 显卡模型",
			mutate: func(r *vm.CreateRequest) {
				r.Arch, r.MachineType, r.Firmware = "aarch64", "virt", "uefi"
				r.VideoModel = "vga"
			},
			wantMsg: "ramfb",
		},
		{
			name:    "x86_64 配 virt 机型",
			mutate:  func(r *vm.CreateRequest) { r.MachineType = "virt" },
			wantMsg: "virt",
		},
		{
			name: "i440fx + UEFI + Windows",
			mutate: func(r *vm.CreateRequest) {
				r.MachineType, r.Firmware, r.OSType = "i440fx", "uefi", "windows"
			},
			wantMsg: "i440fx",
		},
		{
			name: "安全启动配 BIOS",
			mutate: func(r *vm.CreateRequest) {
				r.SecureBoot = true
				r.Firmware = "bios"
			},
			wantMsg: "安全启动",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			req.Name = "combo-" + string(rune('a'+i))
			tc.mutate(&req)
			_, err := svc.Create(ctx, req, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
			if err == nil {
				t.Fatalf("组合 %q 未被拒绝", tc.name)
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("期望业务错误, 实际 %v", err)
			}
			if !strings.Contains(apiErr.Message, tc.wantMsg) {
				t.Errorf("错误文案 %q 未包含 %q", apiErr.Message, tc.wantMsg)
			}
		})
	}
}

// TestCreateAcceptsValidCombinations 覆盖合法组合的放行：联动校验拦的是
// 引导不了的组合，不能把「由模板带出的默认组合」也误伤。
func TestCreateAcceptsValidCombinations(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	// x86_64 的 Windows 组合：UEFI + SATA 磁盘 + e1000 网卡。
	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "win-uefi", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 40,
		OSType: "windows", Firmware: "uefi", MachineType: "q35",
		DiskBus: "sata", NicModel: "e1000", RTCMode: "localtime",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("Windows + UEFI + q35 被误拒: %v", err)
	}

	// aarch64 的完整组合：virt + UEFI + ramfb。
	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "arm-vm", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		Arch: "aarch64", MachineType: "virt", Firmware: "uefi", VideoModel: "ramfb",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("aarch64 + virt + UEFI + ramfb 被误拒: %v", err)
	}

	// 多 ISO：列表优先，单值兜底——两个入口都应受理。
	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "multi-iso", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		ISOFileIDs: []int64{11, 12},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("多 ISO 创建被误拒: %v", err)
	}
}
