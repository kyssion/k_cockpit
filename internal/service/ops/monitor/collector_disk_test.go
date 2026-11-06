package monitor_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// TestSyncRuntimeCorrectsDiskGB 覆盖第 2 轮 NEW-001 的自愈路径。
//
// disk_gb 与 status 是同一类东西：**节点事实的投影**。一次失败的写回、或面板
// 外手工跑的 `qemu-img resize`，都会让它与节点不符——而用户没有任何入口能自己
// 纠正（扩容接口只会向下检查，改不回一个更小的数）。因此域扫描必须同时校正
// 容量，否则任何写入路径上的 bug 都会变成长期错误数据。
//
// 真机证据：任务 119/120/123 成功、节点 11 GiB，而控制面停在 3 GB 且不再变化。
func TestSyncRuntimeCorrectsDiskGB(t *testing.T) {
	db, c := newTestEnv(t, &domainAgent{
		stubClient: newStubClient(),
		domains: []agent.DomainInfo{
			// 节点上的真实容量是 11 GiB。
			{Name: "vm-big", State: model.VMStatusStopped, DiskGB: 11},
			// 域存在但报不出容量：不能据此把容量清成 0。
			{Name: "vm-blank", State: model.VMStatusStopped},
		},
	})

	big := seedVM(t, db, "vm-big", model.VMStatusStopped)
	if err := db.Model(&model.VM{}).Where("id = ?", big.ID).
		Update("disk_gb", 3).Error; err != nil {
		t.Fatalf("预置错误容量失败: %v", err)
	}
	blank := seedVM(t, db, "vm-blank", model.VMStatusStopped)
	if err := db.Model(&model.VM{}).Where("id = ?", blank.ID).
		Update("disk_gb", 40).Error; err != nil {
		t.Fatalf("预置容量失败: %v", err)
	}
	// 域已经不存在的机器：它报不出任何事实，容量必须原样保留。
	gone := seedVM(t, db, "vm-gone", model.VMStatusStopped)
	if err := db.Model(&model.VM{}).Where("id = ?", gone.ID).
		Update("disk_gb", 60).Error; err != nil {
		t.Fatalf("预置容量失败: %v", err)
	}

	c.Tick(context.Background())

	// 每次都用新的接收变量：GORM 的 First 会把 dest 里已有的主键当成附加条件，
	// 复用同一个结构体读第二台机器会得到「record not found」。
	readVM := func(id int64) model.VM {
		t.Helper()
		var v model.VM
		if err := db.First(&v, id).Error; err != nil {
			t.Fatalf("读回虚拟机 %d 失败: %v", id, err)
		}
		return v
	}

	if got := readVM(big.ID); got.DiskGB != 11 {
		t.Errorf("对账应把投影纠正为节点的 11，实际 %d", got.DiskGB)
	}
	if got := readVM(big.ID); got.LastSyncedAt == nil {
		t.Error("对账后应写 last_synced_at")
	}
	if got := readVM(blank.ID); got.DiskGB != 40 {
		t.Errorf("节点报不出容量时不得改写，实际 %d", got.DiskGB)
	}
	if got := readVM(gone.ID); got.DiskGB != 60 {
		t.Errorf("域不存在时不得改写容量，实际 %d", got.DiskGB)
	}
}
