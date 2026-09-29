// e2e-init 为 Playwright 冒烟栈建库：按模型 AutoMigrate 出全套表。
//
// 为什么不用 cmd/migrate：SQL 迁移是 PostgreSQL 专用语法，而冒烟栈用
// 纯 Go SQLite（零外部依赖、每次跑都是干净库）。模型与迁移的结构一致
// 由 database 包的对齐测试兜底。
//
// **仅用于 E2E 冒烟**。生产与常规部署的建库入口是 cmd/migrate；本命令
// 的存在不改变「服务启动不做自动迁移」的约定。
package main

import (
	"log"
	"os"

	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if cfg.DB.Driver != config.DriverSQLite {
		log.Fatalf("e2e-init 只接受 sqlite（冒烟栈专用）；当前 %s", cfg.DB.Driver)
	}
	if _, err := os.Stat(cfg.DB.Path); err == nil {
		log.Fatalf("数据库文件已存在：%s——冒烟栈每次应从干净库开始，请先删除", cfg.DB.Path)
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
	log.Printf("冒烟库就绪: %s", cfg.DB.Path)
}
