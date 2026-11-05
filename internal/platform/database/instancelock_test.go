package database

import (
	"context"
	"path/filepath"
	"testing"

	"k_cockpit/internal/platform/config"
)

// SQLite 是本地开发形态：不启用锁，但必须返回可安全调用的 release，
// 否则 main 里的 defer 会因为没有返回值而退化成空指针。
func TestAcquireInstanceLockSQLiteIsNoop(t *testing.T) {
	db, err := Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "lock.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	release, err := AcquireInstanceLock(context.Background(), db, config.DB{Driver: config.DriverSQLite})
	if err != nil {
		t.Fatalf("SQLite 分支不应失败: %v", err)
	}
	if release == nil {
		t.Fatal("SQLite 分支必须返回可调用的 release")
	}
	release()
}

// 连接池只有 1 条时，锁会独占全部连接，业务查询必然饿死；必须在启动期拒绝。
func TestAcquireInstanceLockRejectsSingleConnPool(t *testing.T) {
	_, err := AcquireInstanceLock(context.Background(), nil, config.DB{
		Driver:       config.DriverPostgres,
		MaxOpenConns: 1,
	})
	if err == nil {
		t.Fatal("MaxOpenConns=1 时应明确拒绝，而不是等到运行期饿死")
	}
}
