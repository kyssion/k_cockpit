// instancelock 防止同一个 PostgreSQL 库被两个控制面实例同时写入。
//
// 进程内的任务队列、实时总线、agent 连接表都不是共享状态：误起第二个实例
// 会重复派发任务、重复执行周期组件，而且两侧都「看起来正常」。锁只解决
// 「双开」，不等于支持多实例部署。
package database

import (
	"context"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/platform/config"
)

// instanceLockKey 是会话级 advisory lock 的固定键。取项目常量而不是按库名
// 派生，保证连到同一个库的所有实例互相排斥（不同库天然不共享锁）。
const instanceLockKey int64 = 0x4b43504b // "KCPK"

// instanceKeepaliveInterval 要明显小于连接池的 ConnMaxLifetime（默认 1h）与
// 服务端 idle timeout，否则那条独占连接会先被关掉，锁随之静默消失。
const instanceKeepaliveInterval = 5 * time.Minute

// AcquireInstanceLock 获取单实例锁，返回的 release 必须由调用方 defer。
//
// **只在 PostgreSQL 生产形态启用**：SQLite 是本地开发/演示用的单写者模型，
// 标准库没有跨平台文件锁，用 PID 文件或 O_EXCL 伪装成锁会在崩溃后留下
// 陈旧锁，比不锁更糟。因此该分支只记日志并放行。
//
// 会话级 advisory lock 绑定后端 session：必须钉住一条独占 *sql.Conn，
// 不能走连接池的普通查询（语句结束连接归还，锁立即失效）。
func AcquireInstanceLock(ctx context.Context, gdb *gorm.DB, cfg config.DB) (func(), error) {
	if cfg.Driver != config.DriverPostgres {
		log.Printf("[instance] driver=%s 不启用单实例锁（仅本地开发/演示形态）", cfg.Driver)
		return func() {}, nil
	}
	if cfg.MaxOpenConns > 0 && cfg.MaxOpenConns < 2 {
		return nil, fmt.Errorf("启用单实例锁需要连接池至少 2 条连接（DB_MAX_OPEN_CONNS=%d）：一条被锁独占，其余留给业务", cfg.MaxOpenConns)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("获取连接池失败: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取独占连接失败: %w", err)
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", instanceLockKey).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("获取单实例锁失败: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		return nil, fmt.Errorf("已有控制面实例持有单实例锁（同一数据库不允许双开）")
	}

	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(instanceKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				var one int
				if err := conn.QueryRowContext(pingCtx, "SELECT 1").Scan(&one); err != nil {
					// 锁随 session 断开而消失。这里不能悄悄继续服务：日志必须
					// 明确指出「双开保护已失效」，由运维决定是否重启。
					log.Printf("[instance] 单实例锁的独占连接保活失败，双开保护可能已失效: %v", err)
				}
				cancel()
			}
		}
	}()

	return func() {
		close(stop)
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", instanceLockKey); err != nil {
			log.Printf("[instance] 释放单实例锁失败（连接关闭后数据库会自行回收）: %v", err)
		}
		_ = conn.Close()
	}, nil
}
