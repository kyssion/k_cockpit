package hub

import (
	"context"
	"path/filepath"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	noderepo "k_cockpit/internal/repository/node"
)

// 节点能力是节点级的，spice_supported 是 VM 级的：心跳必须把两者对齐，
// 包括「能力消失」时把已经置为支持的存量 VM 收回。
func TestOnHeartbeatSyncsSPICESupport(t *testing.T) {
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "spice.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.Node{}, &model.VM{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	if err := db.Create(&model.VM{NodeID: 1, Name: "vm-1", SPICESupported: true}).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	h := &Hub{repo: noderepo.NewRepo(db)}
	ctx := context.Background()
	assert := func(want bool) {
		t.Helper()
		var vm model.VM
		if err := db.Where("name = ?", "vm-1").First(&vm).Error; err != nil {
			t.Fatalf("读取虚拟机失败: %v", err)
		}
		if vm.SPICESupported != want {
			t.Fatalf("spice_supported = %v, 期望 %v", vm.SPICESupported, want)
		}
	}

	// 旧 agent / 未编译 SPICE 的节点：不带标记，存量 VM 必须收回为不支持。
	h.onHeartbeat(ctx, 1, &heartbeat{AgentVersion: "v", ProtocolVersion: 1})
	assert(false)

	h.onHeartbeat(ctx, 1, &heartbeat{Capabilities: []string{agent.CapabilitySPICE}})
	assert(true)

	// 能力消失后再次回退，避免界面留一个点了打不开的入口。
	h.onHeartbeat(ctx, 1, &heartbeat{})
	assert(false)
}
