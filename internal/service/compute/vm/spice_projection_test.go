package vm_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// TestCreateInitializesSPICESupportFromNodeCapabilities 覆盖「节点支持 SPICE，
// 但新建的机器永远开不了 SPICE」。
//
// 实机现象：节点 capabilities 里明确有 console.spice，但新建 VM 的
// GET /vms/:id/console 只有 protocols=["vnc"]，PATCH protocol=spice 直接
// 422「当前节点未上报 SPICE 支持」。
//
// 根因是 spice_supported 只有**心跳**这一个写入方（agent/hub 每 10 秒把
// 节点能力投影到该节点的 VM 行），而新建的 VM 在第一次心跳之前是 false——
// 用户往往就在这一刻去开控制台。因此创建时必须按节点当前能力初始化，
// 之后的漂移仍由心跳收敛。
func TestCreateInitializesSPICESupportFromNodeCapabilities(t *testing.T) {
	client := &probeClient{newStubClient(), model.VMStatusStopped}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	// 节点自报能力（与 agent 心跳写入的是同一列、同一种 JSON 形状）。
	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("capabilities", `["vm.create","console.spice","storage.file.read"]`).Error; err != nil {
		t.Fatalf("写入节点能力失败: %v", err)
	}

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "spice-vm", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("创建受理失败: %v", err)
	}

	row := waitForVMNamed(t, db, "spice-vm")
	if !row.SPICESupported {
		t.Fatal("节点已上报 console.spice，新建的 VM 应为 spice_supported=true（否则开 SPICE 会被 422）")
	}

	// 端到端：控制台协议清单里必须**立刻**有 spice，不必等下一次心跳。
	cfg, err := svc.Console(ctx, row.ID, "", authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("读取控制台配置失败: %v", err)
	}
	found := false
	for _, p := range cfg.Protocols {
		if p == vm.ConsoleProtocolSPICE {
			found = true
		}
	}
	if !found {
		t.Errorf("控制台协议 = %v, 期望包含 spice", cfg.Protocols)
	}
}

// TestCreateWithoutSPICECapabilityKeepsItOff 覆盖反面。
//
// 节点没上报能力时不能"顺手打开"：那会让界面摆出一个点了打不开的协议入口，
// 而用户会以为是网络问题。
func TestCreateWithoutSPICECapabilityKeepsItOff(t *testing.T) {
	client := &probeClient{newStubClient(), model.VMStatusStopped}
	svc, _, db := newTestEnvWithClient(t, client)

	// 节点只有其它能力，没有 console.spice。
	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("capabilities", `["`+agent.CapabilityArchPrefix+`x86_64","vm.create"]`).Error; err != nil {
		t.Fatalf("写入节点能力失败: %v", err)
	}

	if _, err := svc.Create(context.Background(), vm.CreateRequest{
		Name: "vnc-vm", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("创建受理失败: %v", err)
	}

	row := waitForVMNamed(t, db, "vnc-vm")
	if row.SPICESupported {
		t.Error("节点未上报 console.spice，spice_supported 必须保持 false")
	}
}

// TestAdoptInitializesSPICESupport 覆盖纳管路径。
//
// 纳管一台节点上已有的域同样不该等心跳：用户纳管完就会去开控制台。
func TestAdoptInitializesSPICESupport(t *testing.T) {
	client := &probeClient{newStubClient(), model.VMStatusStopped}
	svc, _, db := newTestEnvWithClient(t, client)

	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("capabilities", `["console.spice"]`).Error; err != nil {
		t.Fatalf("写入节点能力失败: %v", err)
	}

	// 桩节点的域清单里有一台 bare-win2022。
	if _, err := svc.AdoptDomain(context.Background(), 1,
		vm.AdoptRequest{Domain: "bare-win2022"},
		authz.Viewer{UserID: 7, IsAdmin: true}, "root", "10.0.0.1"); err != nil {
		t.Fatalf("纳管失败: %v", err)
	}

	var row model.VM
	if err := db.Where("name = ?", "bare-win2022").First(&row).Error; err != nil {
		t.Fatalf("读取纳管记录失败: %v", err)
	}
	if !row.SPICESupported {
		t.Error("纳管时也应按节点当前能力初始化 spice_supported")
	}
}
