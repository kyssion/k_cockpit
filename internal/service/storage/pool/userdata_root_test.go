package pool_test

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/storage/pool"
)

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "pool.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
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
	return db
}

// TestUserdataRootOf 覆盖「用户存储根 = 默认池挂载点/userdata」这条映射。
//
// 实机抓出：根目录此前写死在系统盘，节点建了 15G 数据池也用不上。这里锁住
// 两条边界：默认池存在时根落在池内；没有默认池时返回空（调用方回落历史
// 默认根，而不是猜一个路径）。
func TestUserdataRootOf(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	root, err := pool.UserdataRootOf(ctx, db, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if root != "" {
		t.Errorf("无默认池时应返回空，实际 %q", root)
	}

	mount := "/var/lib/k_cockpit/pools/dev/nbd1"
	cfg := "/var/lib/k_cockpit/pools/dev/nbd2"
	rows := []model.StoragePool{
		{NodeID: 1, DeviceID: "/dev/nbd2", MountPath: &cfg, IsDefault: false, Status: model.StoragePoolReady},
		{NodeID: 1, DeviceID: "/dev/nbd1", MountPath: &mount, IsDefault: true, Status: model.StoragePoolReady},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("造数据失败: %v", err)
		}
	}

	root, err = pool.UserdataRootOf(ctx, db, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if want := mount + "/userdata"; root != want {
		t.Errorf("用户存储根 = %q, 期望 %q", root, want)
	}

	// 默认池被卸载后必须回落：否则新上传会写进一个没有文件系统的挂载点。
	if err := db.Model(&model.StoragePool{}).
		Where("device_id = ?", "/dev/nbd1").
		Update("status", model.StoragePoolUnmounted).Error; err != nil {
		t.Fatal(err)
	}
	root, err = pool.UserdataRootOf(ctx, db, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if root != "" {
		t.Errorf("默认池卸载后应回落为空，实际 %q", root)
	}
}
