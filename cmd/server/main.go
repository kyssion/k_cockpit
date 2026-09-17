// Package main 是 HTTP 服务的入口。
//
// 启动流程：加载配置 -> 连接数据库 -> 装配依赖 -> 注册路由 -> 启动服务。
// 表结构由 internal/database/migrations/ 下的 SQL 迁移管理（见
// docs/02-architecture/DATA_MODEL.md 第 6 节），启动流程不做自动迁移。
package main

import (
	"context"
	"log"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/joho/godotenv"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/database"
	"k_cockpit/internal/network"
	"k_cockpit/internal/node"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/router"
	"k_cockpit/internal/schedule"
	"k_cockpit/internal/settings"
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
	"k_cockpit/internal/template"
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
	riskGuard := risk.NewGuard(db, []byte(cfg.Session.Secret), recorder, cfg.Security.DevBypassCode)
	if riskGuard.DevBypassEnabled() {
		// 醒目地打印：一个只在环境变量里的开关很容易被遗忘，而界面上仍
		// 显示「已绑定验证器」会让所有人以为防护是完整的。
		log.Printf("[risk] ⚠️  开发期万能验证码已启用（SECURITY_DEV_BYPASS_CODE）：" +
			"二次验证可被该固定值直接绕过。**部署到生产前必须清除该配置**" +
			"（生产环境配置它会导致启动失败）。")
	}

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
		// 阶段之间留一点间隔，好让任务时间线能看出推进过程。
		//
		// 测试里用的是零间隔（NewMockClient）：**测试需要确定性，不需要
		// 真实感**——每个用例多等一秒只会让人不愿跑测试。而演示时所有阶段
		// 落在同一毫秒里，时间线虽然是对的，却看不出它是一条时间线。
		mockAgent = agent.NewMockClient().WithStageDelay(220 * time.Millisecond)
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
	// 快照（F-2-07）。三个执行器共用资源锁键 vm:<id>，因此与电源操作天然
	// 互斥——恢复快照时不会有并发的开机请求插进来。
	queue.Register(vm.NewSnapshotCreateExecutor(db, mockAgent))
	queue.Register(vm.NewSnapshotRestoreExecutor(db, mockAgent))
	queue.Register(vm.NewSnapshotDeleteExecutor(db, mockAgent))
	queue.Register(vm.NewConfigUpdateExecutor(db, mockAgent))
	queue.Register(vm.NewEnterRescueExecutor(db, mockAgent))
	queue.Register(vm.NewExitRescueExecutor(db, mockAgent))
	// 模板制备要复制整块系统盘，因此与其它磁盘操作一样走队列。
	queue.Register(template.NewPrepareExecutor(db, mockAgent))
	queue.Register(template.NewDeleteExecutor(db, mockAgent))
	// 网络变更（F-2-03）：三种资源各一个执行器，共用 vm:<id> 资源锁。
	queue.Register(vm.NewInterfaceChangeExecutor(db, mockAgent))
	queue.Register(vm.NewStaticIPChangeExecutor(db, mockAgent))
	queue.Register(vm.NewPortForwardChangeExecutor(db, mockAgent))
	queue.Register(storage.NewCreateExecutor(db, mockAgent))
	queue.Register(storage.NewDeleteExecutor(db, mockAgent))
	// 交换机变更要建网桥，因此与存储池一样走队列。
	queue.Register(network.NewSwitchChangeExecutor(db, mockAgent))
	queue.Start(context.Background())

	settingsSvc := settings.NewService(db, recorder)
	networkSvc := network.NewService(db, mockAgent, queue, recorder)

	// vm 与 storage 都要读设置里的陈旧阈值：把 settingsSvc 作为 Provider
	// 注入，让「面板上改的阈值」真的影响业务行为——否则那两个设置项就是
	// 摆设，而「改了不生效」正是 f-9-01 R-002 要消灭的现象。
	vmSvc := vm.NewService(db, queue, recorder, mockAgent, settingsSvc)
	storageSvc := storage.NewService(db, queue, recorder, mockAgent, settingsSvc)

	// 定时任务（F-7-05）：调度器到点把**已有的任务类型**入队，自己不做任何
	// 节点操作，因此不需要新的执行器——这也是它能在 mock 之上完整跑通的原因。
	scheduleSvc := schedule.NewService(db)
	templateSvc := template.NewService(db, queue, recorder, mockAgent)
	scheduler := schedule.New(db, queue, schedule.Options{})
	scheduler.Start(context.Background())

	// 控制台密码需要可逆加密（f-2-08 R-005）：它要交给 agent 参与 VNC 认证，
	// 因此不能用单向哈希。用途标签与会话签名分开派生。
	vmSvc.SetEncryptionKey(cryptoutil.DeriveKey(
		[]byte(cfg.Session.Secret), "k_cockpit/vm/credential/v1"))

	h := server.Default(server.WithHostPorts(cfg.HTTP.Addr()))
	router.Register(h, router.Deps{
		DB:            db,
		Auth:          authSvc,
		Bootstrap:     bootstrap,
		Node:          nodeSvc,
		VM:            vmSvc,
		Task:          queue,
		Risk:          riskGuard,
		Schedule:      scheduleSvc,
		Template:      templateSvc,
		Storage:       storageSvc,
		Network:       networkSvc,
		Settings:      settingsSvc,
		SecureCookie:  cfg.Session.SecureCookie,
		SimulateAgent: cfg.Agent.Transport == config.AgentTransportMock,
	})

	// Spin 阻塞运行，并在收到 SIGINT / SIGTERM / SIGHUP 时触发优雅退出。
	log.Printf("HTTP 服务启动，监听 %s", cfg.HTTP.Addr())
	h.Spin()
}
