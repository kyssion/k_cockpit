// Package database 负责建立与配置数据库连接。
//
// 同时支持 PostgreSQL 与 SQLite，由配置中的 Driver 决定使用哪一个。
package database

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"k_cockpit/internal/config"
)

// Open 依据配置建立数据库连接，并完成连接池设置与连通性检查。
func Open(cfg config.DB, debug bool) (*gorm.DB, error) {
	dialector, err := dialector(cfg)
	if err != nil {
		return nil, err
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: newLogger(debug),
		// 单条写入本身即原子操作，关闭默认事务可省去一次 BEGIN/COMMIT 往返。
		SkipDefaultTransaction: true,
		// 统一使用单数表名，避免 users 与 user 两套命名并存。
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取底层连接池失败: %w", err)
	}

	if cfg.Driver == config.DriverSQLite {
		// SQLite 是单写者模型，限制为单连接可从根源上避免 "database is locked"。
		sqlDB.SetMaxOpenConns(1)
	} else {
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}

	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("数据库连通性检查失败: %w", err)
	}

	return db, nil
}

// dialector 返回对应驱动的 GORM Dialector，并在需要时准备好 SQLite 的数据目录。
func dialector(cfg config.DB) (gorm.Dialector, error) {
	dsn, err := cfg.DSN()
	if err != nil {
		return nil, err
	}

	switch cfg.Driver {
	case config.DriverPostgres:
		return postgres.Open(dsn), nil

	case config.DriverSQLite:
		// SQLite 不会自动创建父目录，文件所在目录不存在时连接会直接失败。
		if dir := filepath.Dir(cfg.Path); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("创建 SQLite 数据目录 %s 失败: %w", dir, err)
			}
		}
		return sqlite.Open(dsn), nil

	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Driver)
	}
}

// newLogger 构造 GORM 日志器。
//
// 调试环境输出全部 SQL；生产环境只记录错误与慢查询，避免日志量过大。
func newLogger(debug bool) gormlogger.Interface {
	level := gormlogger.Warn
	if debug {
		level = gormlogger.Info
	}

	return gormlogger.New(
		log.New(os.Stdout, "[gorm] ", log.LstdFlags),
		gormlogger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  level,
			IgnoreRecordNotFoundError: true, // 记录不存在是正常业务分支，不作为错误记录
			Colorful:                  false,
		},
	)
}
