// Package main 是 HTTP 服务的入口。
//
// 启动流程：加载配置 -> 连接数据库 -> 装配依赖 -> 注册路由 -> 启动服务。
// 表结构由 internal/database/migrations/ 下的 SQL 迁移管理（见
// docs/02-architecture/DATA_MODEL.md 第 6 节），启动流程不做自动迁移。
package main

import (
	"context"
	"log"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/joho/godotenv"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/node"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/router"
	"k_cockpit/internal/task"
	"k_cockpit/internal/vm"
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
	recorder := audit.NewRecorder(db)
	authSvc := auth.NewService(db, issuer, recorder, auth.Config{
		IdleTimeout:     cfg.Session.IdleTimeout,
		AbsoluteTimeout: cfg.Session.AbsoluteTimeout,
	})

	// 系统尚无管理员时生成一次性初始化令牌。**它只打印到日志**：
	// 能读到日志即等价于拥有服务器访问权，这是「谁有权初始化」的判据。
	// 不采用默认账号密码——那是全网皆知的凭据，存在被抢先登录的窗口。
	// 高风险二次验证的守卫。根密钥复用会话密钥但在内部按用途派生：
	// 单一密钥配置避免部署时多一个必填项，而用途隔离保证签名与加密
	// 不会互相影响。
	riskGuard := risk.NewGuard(db, []byte(cfg.Session.Secret), recorder)

	bootstrap, token, err := auth.NewBootstrap(db, recorder)
	if err != nil {
		log.Fatalf("检查初始化状态失败: %v", err)
	}
	if token != "" {
		log.Printf("\n"+
			"============================================================\n"+
			"  系统尚未初始化，请访问面板创建首个管理员。\n"+
			"  一次性初始化令牌（仅本次运行有效，创建后立即失效）：\n\n"+
			"    %s\n\n"+
			"  提示：该令牌等同于初始化权限，请勿写入公开渠道；\n"+
			"  日志文件的访问权限即初始化权限，请妥善控制。\n"+
			"============================================================", token)
	}

	// 装配 agent 通道。真实实现（gRPC 双向流）尚未开发，本期由 mock 直接
	// 返回结果——业务代码只依赖内部接口，接入真实节点时无需改动（ADR-0007）。
	var mockAgent *agent.MockClient
	switch cfg.Agent.Transport {
	case config.AgentTransportMock:
		mockAgent = agent.NewMockClient()
		log.Printf("[agent] 通道 = mock：节点运行态与领域操作为假数据，不连接真实节点")
	default:
		log.Fatalf("AGENT_TRANSPORT=%s 尚未实现（当前仅支持 %s）",
			cfg.Agent.Transport, config.AgentTransportMock)
	}
	nodeSvc := node.NewService(db, mockAgent, recorder)

	// 任务队列：所有异步操作的载体。注册各能力的 Executor，队列本身
	// 不关心任务具体做什么——新增能力时只需在这里多注册一个。
	queue := task.NewQueue(db, recorder, task.Options{})
	queue.Register(vm.NewCreateExecutor(db, mockAgent))
	queue.Register(vm.NewPowerExecutor(db, mockAgent))
	queue.Register(vm.NewDeleteExecutor(db, mockAgent))
	queue.Start(context.Background())

	vmSvc := vm.NewService(db, queue, recorder, mockAgent)

	h := server.Default(server.WithHostPorts(cfg.HTTP.Addr()))
	router.Register(h, router.Deps{
		DB:            db,
		Auth:          authSvc,
		Bootstrap:     bootstrap,
		Node:          nodeSvc,
		VM:            vmSvc,
		Task:          queue,
		Risk:          riskGuard,
		SecureCookie:  cfg.Session.SecureCookie,
		SimulateAgent: cfg.Agent.Transport == config.AgentTransportMock,
	})

	// Spin 阻塞运行，并在收到 SIGINT / SIGTERM / SIGHUP 时触发优雅退出。
	log.Printf("HTTP 服务启动，监听 %s", cfg.HTTP.Addr())
	h.Spin()
}
