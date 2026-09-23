package handler_test

import (
	"path/filepath"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/handler"
)

// newTestServer 构造测试引擎：注册与生产一致的中间件，但只挂被测路由，
// 避免无关路由干扰。
//
// 数据库使用临时目录下的 SQLite 文件，每个测试互不影响。
// 表结构不在测试库中预建：需要表的测试自行建表，或执行
// internal/database/migrations/ 下的迁移脚本。
func newTestServer(t *testing.T) (*server.Hertz, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "test.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("初始化测试数据库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	h := server.Default()
	h.Use(api.RequestID(), api.Recover())
	h.GET("/health", handler.Health(db))

	return h, db
}
