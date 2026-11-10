package pool_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/storage/pool"
)

func newRefreshEnv(t *testing.T, client *stubClient) (*gorm.DB, *pool.CapacityRefreshLoop) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "refresh.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.StoragePool{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db, pool.NewCapacityRefreshLoop(db, client, nil, pool.CapacityOptions{})
}

func seedPool(t *testing.T, db *gorm.DB, mount string) model.StoragePool {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	row := model.StoragePool{
		NodeID: 1, DeviceID: "/dev/nbd9", MountPath: &mount,
		Status: model.StoragePoolReady, TotalBytes: 999, UsableBytes: 888,
		LastReportedAt: &old,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("建池失败: %v", err)
	}
	return row
}

// TestCapacityRefreshDowngradesUnmountedPool 第 13 轮真机对账：节点重启后
// nbd 设备消失，只剩挂载目录；旧实现把根分区容量当成池容量写回并刷新
// last_reported_at，制造「ready + 新鲜 + 有空间」的假象。
func TestCapacityRefreshDowngradesUnmountedPool(t *testing.T) {
	mount := "/var/lib/k_cockpit/pools/dev/nbd9"
	client := newStubClient()
	client.exec = func(_ context.Context, op agent.Operation) (*agent.Result, error) {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PoolStatListKey: []agent.PoolStat{{Path: mount, Mounted: false}},
		}}, nil
	}
	db, loop := newRefreshEnv(t, client)
	row := seedPool(t, db, mount)

	if err := loop.RunOnce(context.Background()); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	var got model.StoragePool
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StoragePoolUnmounted {
		t.Errorf("未挂载池状态 = %q, 期望 unmounted", got.Status)
	}
	if got.TotalBytes != 999 || got.UsableBytes != 888 {
		t.Errorf("未挂载池容量被改写: total=%d usable=%d", got.TotalBytes, got.UsableBytes)
	}
	if got.LastReportedAt == nil || !got.LastReportedAt.Equal(*row.LastReportedAt) {
		t.Errorf("未挂载池时间戳被刷成最新: %v -> %v", row.LastReportedAt, got.LastReportedAt)
	}
}

// TestCapacityRefreshRecoversWhenMountReturns 降级不是终点：设备重新出现后
// 下一轮刷新要能自动回到 ready 并写入真实容量。
func TestCapacityRefreshRecoversWhenMountReturns(t *testing.T) {
	mount := "/var/lib/k_cockpit/pools/dev/nbd9"
	mounted := false
	client := newStubClient()
	client.exec = func(_ context.Context, _ agent.Operation) (*agent.Result, error) {
		st := agent.PoolStat{Path: mount, Mounted: mounted,
			TotalBytes: 409600, FreeBytes: 204800}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PoolStatListKey: []agent.PoolStat{st},
		}}, nil
	}
	db, loop := newRefreshEnv(t, client)
	row := seedPool(t, db, mount)
	ctx := context.Background()

	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var down model.StoragePool
	_ = db.First(&down, row.ID)
	if down.Status != model.StoragePoolUnmounted {
		t.Fatalf("第一轮未降级: %+v", down)
	}

	mounted = true
	if err := loop.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var up model.StoragePool
	if err := db.First(&up, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if up.Status != model.StoragePoolReady {
		t.Errorf("挂载恢复后未回到 ready: %q", up.Status)
	}
	if up.TotalBytes != 409600 || up.UsableBytes != 204800 {
		t.Errorf("恢复后容量错误: total=%d usable=%d", up.TotalBytes, up.UsableBytes)
	}
	if up.LastReportedAt == nil || !up.LastReportedAt.After(*row.LastReportedAt) {
		t.Errorf("恢复后时间戳未刷新: %v", up.LastReportedAt)
	}
}
