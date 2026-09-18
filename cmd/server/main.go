// Package main 是 HTTP 服务的入口。
//
// 启动流程：加载配置 -> 连接数据库 -> 装配依赖 -> 注册路由 -> 启动服务。
// 表结构由 internal/database/migrations/ 下的 SQL 迁移管理（见
// docs/02-architecture/DATA_MODEL.md 第 6 节），启动流程不做自动迁移。
package main

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/joho/godotenv"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/apikey"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auditlog"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/capture"
	"k_cockpit/internal/config"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/database"
	"k_cockpit/internal/diagnostics"
	"k_cockpit/internal/firewall"
	"k_cockpit/internal/hostfirewall"
	"k_cockpit/internal/hosttuning"
	"k_cockpit/internal/importer"
	"k_cockpit/internal/logging"
	"k_cockpit/internal/monitor"
	"k_cockpit/internal/network"
	"k_cockpit/internal/networkbridge"
	"k_cockpit/internal/node"
	"k_cockpit/internal/passthrough"
	"k_cockpit/internal/platformcheck"
	"k_cockpit/internal/portmirror"
	"k_cockpit/internal/portsecurity"
	"k_cockpit/internal/publicip"
	"k_cockpit/internal/quota"
	"k_cockpit/internal/quotaenforce"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/router"
	"k_cockpit/internal/schedule"
	sched "k_cockpit/internal/scheduler"
	"k_cockpit/internal/securitygroup"
	"k_cockpit/internal/settings"
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
	"k_cockpit/internal/template"
	"k_cockpit/internal/useradmin"
	"k_cockpit/internal/userstorage"
	"k_cockpit/internal/version"
	"k_cockpit/internal/vm"
	"k_cockpit/internal/vmtag"
)

func main() {
	// .env 只服务本地开发；文件不存在时静默忽略，生产环境通过真实环境变量注入。
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	// 日志器要在**任何组件之前**建好：一旦有组件先输出过日志，那些行就
	// 落不到文件里（也不经过脱敏）。而"启动阶段的日志里恰好有初始化令牌"
	// 正是最需要脱敏的一段——见下面打印一次性令牌那里。
	logOpts := logging.DefaultOptions()
	logOpts.Dir = cfg.Log.Dir
	if lv, ok := logging.ParseLevel(cfg.Log.Level); ok {
		logOpts.Level = lv
	}
	logger, err := logging.New(logOpts)
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	defer func() { _ = logger.Close() }()
	// 接管 stdlib log：项目里已有一百多处 log.Printf，逐个改写是一次大范围
	// 改动而收益只是"能按级别过滤"。接管之后它们立刻进入同一个落点
	// （同一份文件、同一个环形缓冲、同一套脱敏规则）。
	log.SetOutput(logger.Writer())

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

	// 装配 agent 通道。gRPC 双向流实现尚未开发，本期由 mock 直接
	// 返回结果——业务代码只依赖内部接口，替换 agent 实现时无需改动（ADR-0007）。
	var mockAgent *agent.MockClient
	switch cfg.Agent.Transport {
	case config.AgentTransportMock:
		// 阶段之间留一点间隔，好让任务时间线能看出推进过程。
		//
		// 测试里用的是零间隔（NewMockClient）：**测试需要确定性，不需要
		// 真实感**——每个用例多等一秒只会让人不愿跑测试。而演示时所有阶段
		// 落在同一毫秒里，时间线虽然是对的，却看不出它是一条时间线。
		mockAgent = agent.NewMockClient().WithStageDelay(220 * time.Millisecond)
		log.Printf("[agent] 通道 = mock：节点运行态与领域操作由 mock 提供，不与节点通信")
	default:
		log.Fatalf("AGENT_TRANSPORT=%s 尚未实现（当前仅支持 %s）",
			cfg.Agent.Transport, config.AgentTransportMock)
	}
	nodeSvc := node.NewService(db, mockAgent, recorder, mockAgent)

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
	// 重装要备份并重建系统盘，同样是磁盘操作。
	queue.Register(vm.NewReinstallExecutor(db, mockAgent))
	queue.Register(vm.NewPurgeExecutor(db, mockAgent))
	// 导出要打包整块磁盘，可能跑到几十分钟。
	queue.Register(vm.NewExportExecutor(db, mockAgent))
	queue.Register(vm.NewExportDeleteExecutor(db, mockAgent))
	// 来宾自动化要进系统内部执行，同样是异步的。
	queue.Register(vm.NewGuestExecutor(db, mockAgent))
	// 镜像导入要转换格式，可能处理几十 GB 的文件。
	queue.Register(importer.NewImportExecutor(db, mockAgent))
	// 迁移要搬运整块磁盘，是最耗时的操作之一。
	queue.Register(vm.NewMigrateExecutor(db, mockAgent))
	// 公网地址变更要动宿主机的 iptables 与路由。
	queue.Register(publicip.NewChangeExecutor(db, mockAgent))
	// 安全组规则要写进宿主机运行域的规则链。
	queue.Register(securitygroup.NewApplyExecutor(db, mockAgent))
	// 目录共享要往虚拟机的域配置里加一块 virtio-9p 设备。
	queue.Register(storage.NewShareExecutor(db, mockAgent))
	// 存储卷要跑 pvcreate/vgcreate/lvcreate，删卷还要逆序释放设备。
	queue.Register(storage.NewVolumeExecutor(db, mockAgent))
	// 端口安全要往节点流表里写规则。
	queue.Register(portsecurity.NewExecutor(db, mockAgent))
	// 抓包：一次限时的抓包，以及删除节点上的抓包文件。
	queue.Register(capture.NewExecutor(db, mockAgent))
	queue.Register(capture.NewDeleteExecutor(db, mockAgent))
	// 配额处置：对某用户的网络施加或撤销限速 / 断网。
	queue.Register(quotaenforce.NewExecutor(db, mockAgent))
	// 宿主机防火墙：应用与紧急回滚。
	queue.Register(hostfirewall.NewExecutor(db, mockAgent))
	// PCIe 直通设备的挂载与卸载。
	queue.Register(passthrough.NewExecutor(db, mockAgent))
	// 宿主机性能调优。
	queue.Register(hosttuning.NewExecutor(db, mockAgent))
	// 平台自检后的重新下发。
	queue.Register(platformcheck.NewExecutor(db, mockAgent))
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
	// 存储配额（f-9-02）：按用户按节点。
	quotaSvc := quota.NewService(db, recorder)
	importerSvc := importer.NewService(db, queue, mockAgent, recorder, quotaSvc)
	publicIPSvc := publicip.NewService(db, queue, mockAgent, recorder)
	sgSvc := securitygroup.NewService(db, queue, mockAgent, recorder)
	// 分片暂存区放在数据库同级的 data 目录下——控制面持久化的东西都在那里。
	chunkStore, err := userstorage.NewChunkStore(filepath.Join(filepath.Dir(cfg.DB.Path), "uploads"))
	if err != nil {
		log.Fatalf("[server] 初始化上传暂存区失败: %v", err)
	}
	userStorageSvc := userstorage.NewService(db, recorder, quotaSvc).
		WithChunks(chunkStore).WithAgent(mockAgent)
	firewallSvc := firewall.NewService(db, mockAgent, recorder)
	apiKeySvc := apikey.NewService(db, recorder)
	mirrorSvc := portmirror.NewService(db, mockAgent, recorder)
	netSvc := networkbridge.NewService(db, mockAgent, recorder)
	auditLogSvc := auditlog.NewService(db)
	userAdminSvc := useradmin.NewService(db, recorder, quotaAdapter{svc: quotaSvc})
	tagSvc := vmtag.NewService(db, recorder)
	// 调度器注册表（F-7-04）：先登记身份与说明，再把记录器交给各组件。
	//
	// **登记发生在启动之前**：界面上的「有哪些调度器在跑」来自这张表，
	// 而不是来自事件表。只靠事件的话，一个正常但最近无事可做的调度器
	// 会从列表里消失——而那与「它坏了」是两回事。
	schedRegistry := sched.NewRegistry()
	sched.RegisterBuiltins(schedRegistry, sched.BuiltinOptions{
		MetricsInterval:        monitor.DefaultOptions().Interval,
		MetricsCleanupInterval: monitor.DefaultOptions().CleanupInterval,
		ScheduleInterval:       schedule.DefaultOptions().Interval,
		QueuePollInterval:      task.DefaultOptions().PollInterval,
		QuotaEvalInterval:      quotaenforce.DefaultOptions().Interval,
	})
	// 记录器在这里声明（在**所有**周期组件之前）：定时任务扫描器在构造之后
	// 马上就要接上它，而采集器要到下面才构造。声明点取两者的最早者，省得
	// 为了一个变量把装配顺序倒过来。
	schedRecorder := sched.NewRecorder(db, schedRegistry)

	monitorSvc := monitor.NewService(db)
	schedulerSvc := sched.NewService(db, schedRegistry)
	portSecuritySvc := portsecurity.NewService(db, mockAgent, recorder, queue)
	captureSvc := capture.NewService(db, mockAgent, recorder, queue)
	quotaEnforceSvc := quotaenforce.NewService(db, queue, mockAgent, recorder)
	// 面板与 SSH 端口作为**合成的保护规则**传给服务——它们跟着配置走，
	// 不存进表：端口改了而表里那条还在，它会保护一个不再监听的端口。
	hostFirewallSvc := hostfirewall.NewService(
		db, queue, mockAgent, recorder, cfg.HTTP.Port, nil)
	// 配额评估循环：配额以月计，5 分钟一轮足够，而它要扫两张按天累计的表。
	quotaLoop := quotaenforce.NewLoop(quotaEnforceSvc, quotaenforce.DefaultOptions())
	quotaLoop.Observe(schedRecorder)
	diagnosticsSvc := diagnostics.NewService(db, settingsSvc, schedRegistry, recorder)
	// 版本摘要进诊断包：排障时第一个要问的就是「跑的是哪个版本」，
	// 而它应当随包一起走，不必再让人回头去问。
	diagnosticsSvc.Version = version.Summary()
	networkSvc := network.NewService(db, mockAgent, queue, recorder)

	// vm 与 storage 都要读设置里的陈旧阈值：把 settingsSvc 作为 Provider
	// 注入，让「面板上改的阈值」真的影响业务行为——否则那两个设置项就是
	// 摆设，而「改了不生效」正是 f-9-01 R-002 要消灭的现象。
	vmSvc := vm.NewService(db, queue, recorder, mockAgent, settingsSvc, quotaSvc)
	storageSvc := storage.NewService(db, queue, recorder, mockAgent, settingsSvc)

	// 定时任务（F-7-05）：调度器到点把**已有的任务类型**入队，自己不做任何
	// 节点操作，因此不需要新的执行器——这也是它能在 mock 之上完整跑通的原因。
	scheduleSvc := schedule.NewService(db)
	templateSvc := template.NewService(db, queue, recorder, mockAgent, quotaSvc)
	scheduler := schedule.New(db, queue, schedule.Options{})
	// 观测要在启动之前接上，否则启动后到接上之间那一轮的动作不会被记录。
	scheduler.Observe(schedRecorder)
	scheduler.Start(context.Background())
	quotaLoop.Start(context.Background())
	defer quotaLoop.Stop()

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
		Quota:         quotaSvc,
		Importer:      importerSvc,
		PublicIP:      publicIPSvc,
		SecurityGroup: sgSvc,
		UserStorage:   userStorageSvc,
		Scheduler:     schedulerSvc,
		PortSecurity:  portSecuritySvc,
		Capture:       captureSvc,
		QuotaEnforce:  quotaEnforceSvc,
		HostFirewall:  hostFirewallSvc,
		Passthrough:   passthrough.NewService(db, queue, mockAgent, recorder),
		HostTuning:    hosttuning.NewService(db, queue, mockAgent, recorder),
		PlatformCheck: platformcheck.NewService(db, queue, mockAgent, recorder),
		Diagnostics:   diagnosticsSvc,
		Firewall:      firewallSvc,
		APIKey:        apiKeySvc,
		PortMirror:    mirrorSvc,
		NetworkBridge: netSvc,
		AuditLog:      auditLogSvc,
		AuditRecorder: recorder,
		Logging:       logger,
		UserAdmin:     userAdminSvc,
		VMTag:         tagSvc,
		Monitor:       monitorSvc,
		Storage:       storageSvc,
		Network:       networkSvc,
		Settings:      settingsSvc,
		SecureCookie:  cfg.Session.SecureCookie,
		SimulateAgent: cfg.Agent.Transport == config.AgentTransportMock,
	})

	// 指标采集器：按固定间隔落库。
	//
	// **它必须独立于页面访问**——「用户看页面时顺便采一次」得到的是密度由
	// 点击行为决定的伪历史：有人看的时候一秒一条，没人看的时候一条都没有。
	// 用它算出来的任何趋势都与真实情况无关，而它看起来像一份正常的图表。
	//
	// 采集有它自己的节奏，与谁在看无关。
	collector := monitor.NewCollector(db, mockAgent, monitor.DefaultOptions())
	collector.Observe(schedRecorder)

	collector.Start(context.Background())
	defer collector.Stop()

	// Spin 阻塞运行，并在收到 SIGINT / SIGTERM / SIGHUP 时触发优雅退出。
	log.Printf("HTTP 服务启动，监听 %s", cfg.HTTP.Addr())
	h.Spin()
}

// quotaAdapter 把 quota 服务适配成 useradmin 需要的最小接口。
//
// 转换发生在装配处（一次），而不是让每个调用点都背上 quota 包的完整依赖。
type quotaAdapter struct{ svc *quota.Service }

func (a quotaAdapter) Set(
	ctx context.Context, req useradmin.QuotaRequest,
	operatorID int64, operatorName, clientIP string,
) error {
	_, err := a.svc.Set(ctx, quota.SetQuotaRequest{
		UserID: req.UserID, NodeID: req.NodeID,
		QuotaBytes: req.QuotaBytes, Enabled: req.Enabled,
	}, operatorID, operatorName, clientIP)
	return err
}
