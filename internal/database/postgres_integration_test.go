package database_test

import (
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"

	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
)

// TestPostgresSchema 验证真实 PostgreSQL 的连接、表结构与初始化状态。
//
// 它是**集成测试**：需要可用的 PostgreSQL（配置来自仓库根目录的 .env）。
// 未配置或连不上时跳过，不影响单元测试在无数据库环境下的运行。
//
// 存在意义：迁移脚本是 PostgreSQL 语法，单元测试用的 SQLite 无法覆盖它——
// 若只在 SQLite 上验证，迁移里的语法错误、字段遗漏要等到部署时才暴露。
func TestPostgresSchema(t *testing.T) {
	if err := godotenv.Load(filepath.Join("..", "..", ".env")); err != nil {
		t.Skip("未找到 .env，跳过 PostgreSQL 集成测试")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用，跳过: %v", err)
	}
	if cfg.DB.Driver != config.DriverPostgres {
		t.Skip("当前未配置 postgres 驱动，跳过")
	}

	db, err := database.Open(cfg.DB, false)
	if err != nil {
		t.Skipf("无法连接 PostgreSQL，跳过: %v", err)
	}
	// 这是集成测试：连的是真实实例，不关连接会一直占用到进程退出。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	var tableCount int64
	if err := db.Raw(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`,
	).Scan(&tableCount).Error; err != nil {
		t.Fatalf("查询表数量失败: %v", err)
	}
	t.Logf("public schema 表数量: %d", tableCount)
	if tableCount < 40 {
		t.Errorf("表数量 = %d, 期望 ≥ 40（迁移 0001 建 43 张表）", tableCount)
	}

	// 认证与审计链路的表必须存在——它们由本次实现直接依赖。
	for _, table := range []string{"user", "user_session", "audit_log", "system_setting", "vm", "node"} {
		var exists bool
		err := db.Raw(
			`SELECT EXISTS (
			   SELECT 1 FROM information_schema.tables
			   WHERE table_schema = 'public' AND table_name = ?
			 )`, table,
		).Scan(&exists).Error
		if err != nil {
			t.Fatalf("查询表 %s 失败: %v", table, err)
		}
		if !exists {
			t.Errorf("表 %s 不存在", table)
		}
	}

	// 迁移登记：确认已执行的迁移版本可查。
	var migrationCount int64
	if err := db.Raw(`SELECT count(*) FROM schema_migration`).Scan(&migrationCount).Error; err != nil {
		t.Errorf("查询迁移登记失败: %v", err)
	} else {
		t.Logf("已执行迁移数量: %d", migrationCount)
	}

	// 初始化状态：本次新增的 Bootstrap 依赖该查询的语义（软删除的管理员不计入）。
	var adminCount int64
	if err := db.Raw(`SELECT count(*) FROM "user" WHERE role = 'admin' AND deleted_at IS NULL`).
		Scan(&adminCount).Error; err != nil {
		t.Fatalf("查询管理员数量失败: %v", err)
	}
	t.Logf("现有管理员数量: %d（为 0 时服务启动会打印初始化令牌）", adminCount)
}
