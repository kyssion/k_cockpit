// Package main 是 HTTP 服务的入口。
//
// 启动流程：加载配置 -> 连接数据库 -> 装配依赖 -> 注册路由 -> 启动服务。
// 表结构由 internal/database/migrations/ 下的 SQL 迁移管理（见
// docs/02-architecture/DATA_MODEL.md 第 6 节），启动流程不做自动迁移。
package main

import (
	"log"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/joho/godotenv"

	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/router"
)

func main() {
	// .env 只服务本地开发；文件不存在时静默忽略，生产环境通过真实环境变量注入。
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("配置加载完成: %s", cfg)

	db, err := database.Open(cfg.DB, cfg.Debug)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}

	// 装配认证。签名密钥强度不足时直接失败启动——带着弱密钥继续运行，
	// 等于把所有会话置于可伪造的风险之下。
	issuer, err := auth.NewTokenIssuer(cfg.Session.Secret)
	if err != nil {
		log.Fatalf("初始化令牌签发器失败: %v", err)
	}
	authSvc := auth.NewService(db, issuer, audit.NewRecorder(db), auth.Config{
		IdleTimeout:     cfg.Session.IdleTimeout,
		AbsoluteTimeout: cfg.Session.AbsoluteTimeout,
	})

	h := server.Default(server.WithHostPorts(cfg.HTTP.Addr()))
	router.Register(h, router.Deps{
		DB:           db,
		Auth:         authSvc,
		SecureCookie: cfg.Session.SecureCookie,
	})

	// Spin 阻塞运行，并在收到 SIGINT / SIGTERM / SIGHUP 时触发优雅退出。
	log.Printf("HTTP 服务启动，监听 %s", cfg.HTTP.Addr())
	h.Spin()
}
