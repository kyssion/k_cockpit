// dev-init 为本地开发初始化 SQLite 库：按 GORM 模型建出全套表。
//
// 与 cmd/migrate 的分工：SQL 迁移是 PostgreSQL 专用语法，生产库一律走
// cmd/migrate；本地开发用纯 Go SQLite，服务启动不做自动迁移，因此需要
// 一个显式的建表入口。与 cmd/e2e-init 的分工：后者要求「每次干净库」，
// 而开发库需要反复执行且不丢数据，AutoMigrate 本身是幂等的。
package main

import (
	"log"

	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg.DB.Driver != config.DriverSQLite {
		log.Fatalf("dev-init 只用于本地 SQLite 开发库；当前驱动 %s 请改用 go run ./cmd/migrate", cfg.DB.Driver)
	}
	db, err := database.Open(cfg.DB, false)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}()
	if err := database.BuildTestSchema(db); err != nil {
		log.Fatalf("建表失败: %v", err)
	}
	log.Printf("开发库就绪: %s（表结构变更仍需重新执行本命令）", cfg.DB.Path)
}
