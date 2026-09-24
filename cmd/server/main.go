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
	"gorm.io/gorm"

	"k_cockpit/internal/accesscontrol"
	"k_cockpit/internal/agent"
	"k_cockpit/internal/alert"
	"k_cockpit/internal/apikey"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auditlog"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authkey"
	"k_cockpit/internal/capture"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/config"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/dashboard"
	"k_cockpit/internal/database"
	"k_cockpit/internal/diagnostics"
	"k_cockpit/internal/firewall"
	"k_cockpit/internal/hostfirewall"
	"k_cockpit/internal/hosttuning"
	"k_cockpit/internal/importer"
	"k_cockpit/internal/invite"
	"k_cockpit/internal/logging"
	"k_cockpit/internal/mailer"
	"k_cockpit/internal/monitor"
	"k_cockpit/internal/network"
	"k_cockpit/internal/networkbridge"
	"k_cockpit/internal/node"
	"k_cockpit/internal/passaudit"
	"k_cockpit/internal/passthrough"
	"k_cockpit/internal/platformcheck"
	"k_cockpit/internal/portmirror"
	"k_cockpit/internal/portsecurity"
	"k_cockpit/internal/publicip"
	"k_cockpit/internal/quota"
	"k_cockpit/internal/quotaenforce"
	"k_cockpit/internal/realtime"
	"k_cockpit/internal/reqlog"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/router"
	"k_cockpit/internal/schedule"
	sched "k_cockpit/internal/scheduler"
	"k_cockpit/internal/search"
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
	"k_cockpit/internal/vpcacl"
)

// app 汇集启动期装配好的全部依赖。
//
// main 曾是一个 450 行的函数，装配顺序与跨服务接线全挤在一起，读它要一次性
// 记住五十多个局部变量。用一个结构体承载这些依赖、按模块拆成若干 setup* 方法
// 之后，main 只剩「按序装配 -> 启动 -> 注册路由」的骨架，各模块内部的接线细节
// 收敛到对应方法里。
//
// 方法之间的**调用顺序就是装配顺序**，其中若干处有硬约束（日志最先、总线先于
// 队列、调度记录器先于所有周期组件、加密密钥在受理任何任务前注入）。这些约束
// 原本靠"变量在文件里的先后位置"隐式表达，现在集中在 main 的调用序列里，一眼
// 可见；跨方法的依赖通过结构体字段传递，因此每个 setup* 只依赖在它之前调用过
// 的方法已经填好的字段。
type app struct {
	cfg    config.Config
	logger *logging.Logger
	db     *gorm.DB

	// 认证与安全
	issuer     *auth.TokenIssuer
	recorder   *audit.Recorder
	authKeySvc *authkey.Service
	authSvc    *auth.Service
	riskGuard  *risk.Guard
	bootstrap  *auth.Bootstrap

	// 节点通道
	mockAgent *agent.MockClient
	nodeSvc   *node.Service

	// 任务队列。createExec / guestExec 单独留字段：它们要在 setupCredentials
	// 里补注入加密密钥，而那发生在队列注册之后。
	bus        *realtime.Bus
	queue      *task.Queue
	createExec *vm.CreateExecutor
	guestExec  *vm.GuestExecutor

	// 业务服务
	settingsSvc     *settings.Service
	reqLogSvc       *reqlog.Service
	passAuditSvc    *passaudit.Service
	mailSvc         *mailer.Service
	quotaSvc        *quota.Service
	importerSvc     *importer.Service
	publicIPSvc     *publicip.Service
	sgSvc           *securitygroup.Service
	userStorageSvc  *userstorage.Service
	firewallSvc     *firewall.Service
	apiKeySvc       *apikey.Service
	mirrorSvc       *portmirror.Service
	netSvc          *networkbridge.Service
	auditLogSvc     *auditlog.Service
	userAdminSvc    *useradmin.Service
	inviteSvc       *invite.Service
	tagSvc          *vmtag.Service
	monitorSvc      *monitor.Service
	hostTuningSvc   *hosttuning.Service
	dashboardSvc    *dashboard.Service
	schedulerSvc    *sched.Service
	portSecuritySvc *portsecurity.Service
	captureSvc      *capture.Service
	quotaEnforceSvc *quotaenforce.Service
	hostFirewallSvc *hostfirewall.Service
	diagnosticsSvc  *diagnostics.Service
	networkSvc      *network.Service
	vmSvc           *vm.Service
	computeQuotaSvc *computequota.Service
	alertSvc        *alert.Service
	storageSvc      *storage.Service
	scheduleSvc     *schedule.Service
	templateSvc     *template.Service

	// 调度器注册表与记录器：周期组件共用，必须先于它们构造。
	schedRegistry *sched.Registry
	schedRecorder *sched.Recorder

	// 后台周期组件
	scheduler     *schedule.Scheduler
	quotaLoop     *quotaenforce.Loop
	alertLoop     *alert.Loop
	passAuditLoop *passaudit.Loop
	authKeyLoop   *authkey.Loop
	collector     *monitor.Collector
}

func main() {
	a := &app{}

	// 装配阶段：顺序不可调换，理由见 app 与各 setup* 方法的注释。
	a.loadConfig()
	a.setupLogging()
	// 日志器要在**任何组件之前**建好（见 setupLogging），因此它的关闭也排在
	// 最后：defer 先注册者后执行，这一条最早注册，正好最后关闭。
	defer func() { _ = a.logger.Close() }()
	a.openDB()

	a.setupAuth()
	a.setupAgent()
	a.setupTaskQueue()
	a.setupSchedulerRegistry()
	a.setupServices()
	a.setupCredentials()
	a.setupBackground()

	h := a.newHTTPServer()
	a.registerRoutes(h)

	// 周期组件的启动放在路由注册之后：它们都在独立的定时 goroutine 上，
	// 与路由注册互不依赖，而服务在 h.Spin() 之前不会受理任何请求。
	a.startBackground()
	// stopBackground 后注册，先于 logger.Close 执行——与拆分前三个 defer
	// （collector / alertLoop / quotaLoop）的 LIFO 顺序一致。
	defer a.stopBackground()

	// Spin 阻塞运行，并在收到 SIGINT / SIGTERM / SIGHUP 时触发优雅退出。
	log.Printf("HTTP 服务启动，监听 %s", a.cfg.HTTP.Addr())
	h.Spin()
}

// loadConfig 加载 .env 与应用配置。
func (a *app) loadConfig() {
	// .env 只服务本地开发；文件不存在时静默忽略，生产环境通过真实环境变量注入。
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	a.cfg = cfg
}

// setupLogging 建好日志器并接管 stdlib log。
//
// 必须在任何组件之前调用：一旦有组件先输出过日志，那些行就落不到文件里
// （也不经过脱敏）。而"启动阶段的日志里恰好有初始化令牌"正是最需要脱敏的
// 一段——见 setupAuth 里打印一次性令牌那里。
func (a *app) setupLogging() {
	logOpts := logging.DefaultOptions()
	logOpts.Dir = a.cfg.Log.Dir
	if lv, ok := logging.ParseLevel(a.cfg.Log.Level); ok {
		logOpts.Level = lv
	}
	logger, err := logging.New(logOpts)
	if err != nil {
		log.Fatalf("初始化日志失败: %v", err)
	}
	a.logger = logger
	// 接管 stdlib log：项目里已有一百多处 log.Printf，逐个改写是一次大范围
	// 改动而收益只是"能按级别过滤"。接管之后它们立刻进入同一个落点
	// （同一份文件、同一个环形缓冲、同一套脱敏规则）。
	log.SetOutput(logger.Writer())

	log.Printf("配置加载完成: %s", a.cfg)
}

// openDB 打开数据库连接。
func (a *app) openDB() {
	db, err := database.Open(a.cfg.DB, a.cfg.Debug)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	a.db = db
}

// setupAuth 装配认证、审计记录器、会话密钥轮换、高风险守卫与初始化引导。
func (a *app) setupAuth() {
	// 装配认证。签名密钥强度不足时直接失败启动——带着弱密钥继续运行，
	// 等于把所有会话置于可伪造的风险之下。
	issuer, err := auth.NewTokenIssuer(a.cfg.Session.Secret)
	if err != nil {
		log.Fatalf("初始化令牌签发器失败: %v", err)
	}
	a.issuer = issuer
	a.recorder = audit.NewRecorder(a.db)

	// 会话签名密钥（F-1-09）：库里没有时用配置里的初始密钥建一条，之后轮换
	// 直接换库里的记录——换一次环境变量再重启的做法，在"怀疑泄漏"的场景下
	// 没有可用的时间窗。
	a.authKeySvc = authkey.NewService(a.db, cryptoutil.DeriveKey(
		[]byte(a.cfg.Session.Secret), "k_cockpit/auth_key/v1"), a.cfg.Session.Secret, a.recorder)
	issuer.SetKeyProvider(func() (string, []byte, error) {
		return a.authKeySvc.Load(context.Background())
	})

	a.authSvc = auth.NewService(a.db, issuer, a.recorder, auth.Config{
		IdleTimeout:     a.cfg.Session.IdleTimeout,
		AbsoluteTimeout: a.cfg.Session.AbsoluteTimeout,
	})

	// 高风险二次验证的守卫。根密钥复用会话密钥但在内部按用途派生：
	// 单一密钥配置避免部署时多一个必填项，而用途隔离保证签名与加密
	// 不会互相影响。
	a.riskGuard = risk.NewGuard(a.db, []byte(a.cfg.Session.Secret), a.recorder, a.cfg.Security.DevBypassCode)
	if a.riskGuard.DevBypassEnabled() {
		// 醒目地打印：一个只在环境变量里的开关很容易被遗忘，而界面上仍
		// 显示「已绑定验证器」会让所有人以为防护是完整的。
		log.Printf("[risk] ⚠️  开发期万能验证码已启用（SECURITY_DEV_BYPASS_CODE）：" +
			"二次验证可被该固定值直接绕过。**部署到生产前必须清除该配置**" +
			"（生产环境配置它会导致启动失败）。")
	}

	// 系统尚无管理员时生成一次性初始化令牌。**它只打印到日志**：
	// 能读到日志即等价于拥有服务器访问权，这是「谁有权初始化」的判据。
	// 不采用默认账号密码——那是全网皆知的凭据，存在被抢先登录的窗口。
	bootstrap, token, err := auth.NewBootstrap(a.db, a.recorder)
	if err != nil {
		log.Fatalf("检查初始化状态失败: %v", err)
	}
	a.bootstrap = bootstrap
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
}

// setupAgent 装配节点通道与节点服务。
func (a *app) setupAgent() {
	// 装配 agent 通道。gRPC 双向流实现尚未开发，本期由 mock 直接
	// 返回结果——业务代码只依赖内部接口，替换 agent 实现时无需改动（ADR-0007）。
	switch a.cfg.Agent.Transport {
	case config.AgentTransportMock:
		// 阶段之间留一点间隔，好让任务时间线能看出推进过程。
		//
		// 测试里用的是零间隔（NewMockClient）：**测试需要确定性，不需要
		// 真实感**——每个用例多等一秒只会让人不愿跑测试。而演示时所有阶段
		// 落在同一毫秒里，时间线虽然是对的，却看不出它是一条时间线。
		a.mockAgent = agent.NewMockClient().WithStageDelay(220 * time.Millisecond)
		log.Printf("[agent] 通道 = mock：节点运行态与领域操作由 mock 提供，不与节点通信")
	default:
		log.Fatalf("AGENT_TRANSPORT=%s 尚未实现（当前仅支持 %s）",
			a.cfg.Agent.Transport, config.AgentTransportMock)
	}
	a.nodeSvc = node.NewService(a.db, a.mockAgent, a.recorder, a.mockAgent)
}

// setupTaskQueue 建实时总线与任务队列，注册全部执行器并启动调度循环。
func (a *app) setupTaskQueue() {
	db, mockAgent := a.db, a.mockAgent

	// 实时事件总线：任务状态变化由队列在关键节点广播，SSE 端点订阅它。
	//
	// 先建总线再建队列，是因为队列要在入队、派发与落定三处发布事件——
	// 顺序反了就只能事后补一次装配，而那种"可选装配"最容易被忘记。
	a.bus = realtime.NewBus()
	// 任务队列：所有异步操作的载体。注册各能力的 Executor，队列本身
	// 不关心任务具体做什么——新增能力时只需在这里多注册一个。
	a.queue = task.NewQueue(db, a.recorder, task.Options{}).WithBus(a.bus)
	a.createExec = vm.NewCreateExecutor(db, mockAgent)
	a.queue.Register(a.createExec)
	a.queue.Register(vm.NewPowerExecutor(db, mockAgent))
	a.queue.Register(vm.NewDeleteExecutor(db, mockAgent))
	// 快照（F-2-07）。三个执行器共用资源锁键 vm:<id>，因此与电源操作天然
	// 互斥——恢复快照时不会有并发的开机请求插进来。
	a.queue.Register(vm.NewSnapshotCreateExecutor(db, mockAgent))
	a.queue.Register(vm.NewSnapshotRestoreExecutor(db, mockAgent))
	a.queue.Register(vm.NewSnapshotDeleteExecutor(db, mockAgent))
	// 批量删除快照与 UEFI 启动项修复（F-2-07 / F-2-11）。
	a.queue.Register(vm.NewSnapshotDeleteAllExecutor(db, mockAgent))
	a.queue.Register(vm.NewNVRAMRepairExecutor(mockAgent))
	// ACL 应用（F-4-05）。
	a.queue.Register(vpcacl.NewExecutor(mockAgent))
	a.queue.Register(vm.NewConfigUpdateExecutor(db, mockAgent))
	a.queue.Register(vm.NewEnterRescueExecutor(db, mockAgent))
	a.queue.Register(vm.NewExitRescueExecutor(db, mockAgent))
	// 模板制备要复制整块系统盘，因此与其它磁盘操作一样走队列。
	a.queue.Register(template.NewPrepareExecutor(db, mockAgent))
	a.queue.Register(template.NewDeleteExecutor(db, mockAgent))
	// 模板导出与导入（F-3-05）：打包与解包都要读写整块镜像。
	a.queue.Register(template.NewExportExecutor(db, mockAgent))
	a.queue.Register(template.NewExportDeleteExecutor(db, mockAgent))
	a.queue.Register(template.NewImportExecutor(db, mockAgent))
	// 派生链维护（rebase / 拉平 / 提升 / 热提升删除）。
	a.queue.Register(template.NewMaintainExecutor(db, mockAgent))
	// 离线预处理（F-3-06）。
	a.queue.Register(template.NewPreprocessExecutor(db, mockAgent))
	// 重装要备份并重建系统盘，同样是磁盘操作。
	a.queue.Register(vm.NewReinstallExecutor(db, mockAgent))
	a.queue.Register(vm.NewPurgeExecutor(db, mockAgent))
	// 导出要打包整块磁盘，可能跑到几十分钟。
	a.queue.Register(vm.NewExportExecutor(db, mockAgent))
	a.queue.Register(vm.NewExportDeleteExecutor(db, mockAgent))
	// 来宾自动化要进系统内部执行，同样是异步的。
	// （改密成功后要同步凭据记录，需要加密密钥——在 setupCredentials 处注入。）
	a.guestExec = vm.NewGuestExecutor(db, mockAgent)
	a.queue.Register(a.guestExec)
	// 镜像导入要转换格式，可能处理几十 GB 的文件。
	a.queue.Register(importer.NewImportExecutor(db, mockAgent))
	// 迁移要搬运整块磁盘，是最耗时的操作之一。
	a.queue.Register(vm.NewMigrateExecutor(db, mockAgent))
	// 公网地址变更要动宿主机的 iptables 与路由。
	a.queue.Register(publicip.NewChangeExecutor(db, mockAgent))
	// 安全组规则要写进宿主机运行域的规则链。
	a.queue.Register(securitygroup.NewApplyExecutor(db, mockAgent))
	// 目录共享要往虚拟机的域配置里加一块 virtio-9p 设备。
	a.queue.Register(storage.NewShareExecutor(db, mockAgent))
	// 存储卷要跑 pvcreate/vgcreate/lvcreate，删卷还要逆序释放设备。
	a.queue.Register(storage.NewVolumeExecutor(db, mockAgent))
	// 端口安全要往节点流表里写规则。
	a.queue.Register(portsecurity.NewExecutor(db, mockAgent))
	// 抓包：一次限时的抓包，以及删除节点上的抓包文件。
	a.queue.Register(capture.NewExecutor(db, mockAgent))
	a.queue.Register(capture.NewDeleteExecutor(db, mockAgent))
	// 配额处置：对某用户的网络施加或撤销限速 / 断网。
	a.queue.Register(quotaenforce.NewExecutor(db, mockAgent))
	// 宿主机防火墙：应用与紧急回滚。
	a.queue.Register(hostfirewall.NewExecutor(db, mockAgent))
	// PCIe 直通设备的挂载与卸载。
	a.queue.Register(passthrough.NewExecutor(db, mockAgent))
	// 宿主机性能调优。
	a.queue.Register(hosttuning.NewExecutor(db, mockAgent))
	// 平台自检后的重新下发。
	a.queue.Register(platformcheck.NewExecutor(db, mockAgent))
	// 关机状态下的磁盘扩容。
	a.queue.Register(vm.NewDiskResizeExecutor(db, mockAgent))
	// 磁盘的挂载 / 卸载 / 换总线（F-2-06）。
	a.queue.Register(vm.NewDiskChangeExecutor(db, mockAgent))
	// 链接克隆的磁盘合并为独立镜像。
	a.queue.Register(vm.NewIndependentExecutor(db, mockAgent))
	// 光驱（挂载 / 弹出 / 换盘 / 摘除 / 换总线）。
	a.queue.Register(vm.NewCDROMExecutor(db, mockAgent))
	// 网络变更（F-2-03）：三种资源各一个执行器，共用 vm:<id> 资源锁。
	a.queue.Register(vm.NewInterfaceChangeExecutor(db, mockAgent))
	a.queue.Register(vm.NewStaticIPChangeExecutor(db, mockAgent))
	a.queue.Register(vm.NewPortForwardChangeExecutor(db, mockAgent))
	a.queue.Register(storage.NewCreateExecutor(db, mockAgent))
	a.queue.Register(storage.NewDeleteExecutor(db, mockAgent))
	// 分区、池配置与卸载（F-5-01 后续迭代）。
	a.queue.Register(storage.NewPartitionExecutor(mockAgent))
	a.queue.Register(storage.NewPartitionDeleteExecutor(mockAgent))
	a.queue.Register(storage.NewPoolConfigExecutor(db, mockAgent))
	a.queue.Register(storage.NewPoolUnmountExecutor(db, mockAgent))
	// 交换机变更要建网桥，因此与存储池一样走队列。
	a.queue.Register(network.NewSwitchChangeExecutor(db, mockAgent))
	a.queue.Start(context.Background())
}

// setupSchedulerRegistry 登记调度器身份并建记录器。
//
// 必须先于所有周期组件（setupBackground）调用：它们都要 Observe 这个记录器。
func (a *app) setupSchedulerRegistry() {
	// 调度器注册表（F-7-04）：先登记身份与说明，再把记录器交给各组件。
	//
	// **登记发生在启动之前**：界面上的「有哪些调度器在跑」来自这张表，
	// 而不是来自事件表。只靠事件的话，一个正常但最近无事可做的调度器
	// 会从列表里消失——而那与「它坏了」是两回事。
	a.schedRegistry = sched.NewRegistry()
	sched.RegisterBuiltins(a.schedRegistry, sched.BuiltinOptions{
		MetricsInterval:        monitor.DefaultOptions().Interval,
		MetricsCleanupInterval: monitor.DefaultOptions().CleanupInterval,
		ScheduleInterval:       schedule.DefaultOptions().Interval,
		QueuePollInterval:      task.DefaultOptions().PollInterval,
		QuotaEvalInterval:      quotaenforce.DefaultOptions().Interval,
		PasswordAuditInterval:  24 * time.Hour,
	})
	// 记录器在这里建好（在**所有**周期组件之前）：定时任务扫描器与采集器
	// 构造之后马上就要接上它。
	a.schedRecorder = sched.NewRecorder(a.db, a.schedRegistry)
}

// setupServices 按域装配全部业务服务并完成它们之间的接线。
func (a *app) setupServices() {
	a.setupSettingsServices()
	a.setupResourceServices()
	a.setupObservabilityServices()
	a.setupComputeServices()
}

// setupSettingsServices 装配设置、请求日志、口令检查与邮件。
func (a *app) setupSettingsServices() {
	db, mockAgent := a.db, a.mockAgent

	a.settingsSvc = settings.NewService(db, a.recorder)
	// 请求日志的开关来自设置项：每次写入前读取，因此改了设置立即生效，
	// 不需要重启（重启才能生效会让人以为开关坏了）。
	a.reqLogSvc = reqlog.NewService(db, func(ctx context.Context) bool {
		return a.settingsSvc.Bool("security.request_log_enabled", false)
	})

	// 口令检查：判定在节点侧，这里只做开关、定时与结果落地。
	a.passAuditSvc = passaudit.NewService(db, mockAgent, func() bool {
		return a.settingsSvc.Bool("security.password_breach_check", false)
	}, a.recorder)
	// 邮件（F-1-08）：配置来自系统设置，因此管理员保存 SMTP 后**立即**生效，
	// 无需重启——"测试邮件"按钮是验证配置是否正确的唯一手段，重启才能生效
	// 会让那个按钮看起来一直是坏的。
	a.mailSvc = mailer.New(a.settingsSvc.MailConfig)
	// 登录阶段的二次验证复用 risk 的校验逻辑（恢复码一次性、TOTP 容差），
	// 接线在 router.Register 内完成——漏接的表现是"登录时永远提示服务
	// 不可用"，而编译期看不出来。
	a.authSvc.SetMailer(a.mailSvc)
}

// setupResourceServices 装配配额、导入、公网、安全组、用户存储、用户与邀请等资源类服务。
func (a *app) setupResourceServices() {
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	// 存储配额（f-9-02）：按用户按节点。
	a.quotaSvc = quota.NewService(db, a.recorder)
	a.importerSvc = importer.NewService(db, queue, mockAgent, a.recorder, a.quotaSvc)
	a.publicIPSvc = publicip.NewService(db, queue, mockAgent, a.recorder)
	a.sgSvc = securitygroup.NewService(db, queue, mockAgent, a.recorder)
	// 分片暂存区放在数据库同级的 data 目录下——控制面持久化的东西都在那里。
	chunkStore, err := userstorage.NewChunkStore(filepath.Join(filepath.Dir(a.cfg.DB.Path), "uploads"))
	if err != nil {
		log.Fatalf("[server] 初始化上传暂存区失败: %v", err)
	}
	a.userStorageSvc = userstorage.NewService(db, a.recorder, a.quotaSvc).
		WithChunks(chunkStore).WithAgent(mockAgent)
	a.firewallSvc = firewall.NewService(db, mockAgent, a.recorder)
	a.apiKeySvc = apikey.NewService(db, a.recorder)
	a.mirrorSvc = portmirror.NewService(db, mockAgent, a.recorder)
	a.netSvc = networkbridge.NewService(db, mockAgent, a.recorder)
	a.auditLogSvc = auditlog.NewService(db)
	a.userAdminSvc = useradmin.NewService(db, a.recorder, quotaAdapter{svc: a.quotaSvc})
	// 邀请注册（F-1-10）。账号创建复用 useradmin（密码由受邀人自设），链接里的站点地址取自设置项——没有配置时给出相对链接，由管理员自己补域名。
	a.inviteSvc = invite.NewService(db, a.recorder)
	a.inviteSvc.SetUserCreator(a.userAdminSvc.CreateFromInvite)
	a.inviteSvc.SetMailer(func(ctx context.Context, to, link, role string) error {
		// role 直接进正文：受邀人需要知道自己被邀请成什么角色。
		return a.mailSvc.Send(ctx, to, "邀请你加入 K Cockpit",
			"你被邀请加入 K Cockpit，角色："+role+"\n\n"+
				"请打开下面的链接完成注册（链接三天内有效）：\n"+link+"\n\n"+
				"如果你并不认识邀请你的人，请忽略这封邮件。\n")
	})
	// 站点地址留空 —— 链接是相对路径（/invite/<token>）。邮件里给相对链接比猜一个错误域名更安全：管理员转发时自己补域名。
	a.inviteSvc.SetSiteURL(func(ctx context.Context) string { return "" })
	// SSH 访问要下发到宿主机，因此接上 agent（不接时只改控制面记录）。
	a.userAdminSvc.SetAgent(mockAgent)
	a.tagSvc = vmtag.NewService(db, a.recorder)
}

// setupObservabilityServices 装配监控、工作台、调度器视图、网络与诊断等只读聚合类服务。
func (a *app) setupObservabilityServices() {
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	a.monitorSvc = monitor.NewService(db)
	// 宿主机调优（KSM / zRAM / 嵌套虚拟化）。工作台要复用它读一次状态，
	// 因此先建出来而不是塞进 Deps 里现造——两个实例意味着两份缓存口径。
	a.hostTuningSvc = hosttuning.NewService(db, queue, mockAgent, a.recorder)

	// 工作台概览（F-8-03 / F-8-04）：只读聚合，节点运行态复用节点服务。
	a.dashboardSvc = dashboard.NewService(db, a.nodeSvc)
	// 工作台上的 KSM / zRAM 直接读调优服务（同一份口径），硬件与网络统计
	// 走 agent 的两个按需操作。
	a.dashboardSvc.SetTuning(a.hostTuningSvc)
	a.dashboardSvc.SetAgent(mockAgent)
	a.schedulerSvc = sched.NewService(db, a.schedRegistry)
	a.portSecuritySvc = portsecurity.NewService(db, mockAgent, a.recorder, queue)
	a.captureSvc = capture.NewService(db, mockAgent, a.recorder, queue)
	a.quotaEnforceSvc = quotaenforce.NewService(db, queue, mockAgent, a.recorder)
	// 面板与 SSH 端口作为**合成的保护规则**传给服务——它们跟着配置走，
	// 不存进表：端口改了而表里那条还在，它会保护一个不再监听的端口。
	a.hostFirewallSvc = hostfirewall.NewService(
		db, queue, mockAgent, a.recorder, a.cfg.HTTP.Port, nil)
	a.diagnosticsSvc = diagnostics.NewService(db, a.settingsSvc, a.schedRegistry, a.recorder)
	// 版本摘要进诊断包：排障时第一个要问的就是「跑的是哪个版本」，
	// 而它应当随包一起走，不必再让人回头去问。
	a.diagnosticsSvc.Version = version.Summary()
	a.networkSvc = network.NewService(db, mockAgent, queue, a.recorder)
}

// setupComputeServices 装配虚拟机、计算配额、存储、模板与定时任务等计算类服务。
func (a *app) setupComputeServices() {
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	// vm 与 storage 都要读设置里的陈旧阈值：把 settingsSvc 作为 Provider
	// 注入，让「面板上改的阈值」真的影响业务行为——否则那两个设置项就是
	// 摆设，而「改了不生效」正是 f-9-01 R-002 要消灭的现象。
	a.vmSvc = vm.NewService(db, queue, a.recorder, mockAgent, a.settingsSvc, a.quotaSvc)
	// 计算资源配额（vCPU / 内存 / 实例数）：创建虚拟机时校验，超限即拒绝新建。
	a.computeQuotaSvc = computequota.NewService(db, a.recorder)
	// 工作台「我的配额」（G-32）：三类配额的读数都从各自的判定服务取，
	// 保证用户看到的数字与判定时用的是同一份。
	a.dashboardSvc.SetQuotaReaders(a.computeQuotaSvc, a.quotaSvc, a.quotaEnforceSvc)
	// 告警中心（F-8-07）。
	a.alertSvc = alert.NewService(db)
	a.vmSvc.SetComputeQuota(a.computeQuotaSvc)
	// 列表要带标签与最近占用，两者都是**一次批量查询**；不装配则列表不带这两列。
	a.vmSvc.SetTagProvider(a.tagSvc)
	// 公网地址与端口转发的数量同样受计算配额约束：它们都是稀缺资源，
	// 而"先到先得"通常不是管理员想要的分配策略。
	a.publicIPSvc.SetComputeQuota(a.computeQuotaSvc)
	a.storageSvc = storage.NewService(db, queue, a.recorder, mockAgent, a.settingsSvc)

	// 定时任务（F-7-05）：调度器到点把**已有的任务类型**入队，自己不做任何
	// 节点操作，因此不需要新的执行器——这也是它能在 mock 之上完整跑通的原因。
	a.scheduleSvc = schedule.NewService(db)
	a.templateSvc = template.NewService(db, queue, a.recorder, mockAgent, a.quotaSvc)
}

// setupCredentials 给需要可逆凭据的服务注入加密密钥。
//
// 必须在受理任何任务之前完成：创建与来宾改密都要用它加密初始凭据。
func (a *app) setupCredentials() {
	// 控制台密码需要可逆加密（f-2-08 R-005）：它要交给 agent 参与 VNC 认证，
	// 因此不能用单向哈希。用途标签与会话签名分开派生。
	// 控制台密码与初始登录密码共用同一把派生密钥：它们都是"交给节点或展示
	// 给用户的可逆凭据"，分开派生只会让"忘了配哪一个"的排查面翻倍。
	credKey := cryptoutil.DeriveKey(
		[]byte(a.cfg.Session.Secret), "k_cockpit/vm/credential/v1")
	a.vmSvc.SetEncryptionKey(credKey)
	a.createExec.SetEncryptionKey(credKey)
	a.guestExec.SetEncryptionKey(credKey)
}

// setupBackground 构造全部后台周期组件并接上调度记录器。
//
// 只构造与 Observe，不 Start——启动集中在 startBackground，以便与优雅退出
// （stopBackground）成对出现。观测必须在启动之前接上，否则启动后到接上之间
// 那一轮的动作不会被记录。
func (a *app) setupBackground() {
	// 定时任务扫描器。定时快照复用 vm 服务的创建快照入口：那条路要先建记录
	// 拿 ID、过配额、探测运行态。在调度器里重抄一遍等于把规则放两份。
	a.scheduler = schedule.New(a.db, a.queue, schedule.Options{})
	a.scheduler.SetSnapshotCreator(a.vmSvc)
	a.scheduler.Observe(a.schedRecorder)

	// 配额评估循环：配额以月计，5 分钟一轮足够，而它要扫两张按天累计的表。
	a.quotaLoop = quotaenforce.NewLoop(a.quotaEnforceSvc, quotaenforce.DefaultOptions())
	a.quotaLoop.Observe(a.schedRecorder)

	// 告警评估循环（F-8-07）：只读库表，不探测节点——评估每五分钟跑一次，
	// 在里面探测会让告警系统自己成为负载。
	a.alertLoop = alert.NewLoop(a.alertSvc, alert.DefaultOptions())
	a.alertLoop.Observe(a.schedRecorder)

	// 口令安全检查循环（F-10-06）：一天一次，判定在节点侧完成。
	a.passAuditLoop = passaudit.NewLoop(a.passAuditSvc, passaudit.DefaultOptions())
	a.passAuditLoop.Observe(a.schedRecorder)

	// 会话密钥自动轮换（F-1-09）：间隔为 0 时这个循环什么都不做。
	a.authKeyLoop = authkey.NewLoop(a.authKeySvc, func() int {
		return a.settingsSvc.Int("security.auth_key_rotate_days", 0)
	}, authkey.DefaultOptions())
	a.authKeyLoop.Observe(a.schedRecorder)

	// 指标采集器：按固定间隔落库。
	//
	// **它必须独立于页面访问**——「用户看页面时顺便采一次」得到的是密度由
	// 点击行为决定的伪历史：有人看的时候一秒一条，没人看的时候一条都没有。
	// 用它算出来的任何趋势都与真实情况无关，而它看起来像一份正常的图表。
	//
	// 采集有它自己的节奏，与谁在看无关。
	a.collector = monitor.NewCollector(a.db, a.mockAgent, monitor.DefaultOptions())
	a.collector.Observe(a.schedRecorder)
}

// startBackground 启动全部后台周期组件。
func (a *app) startBackground() {
	ctx := context.Background()
	a.scheduler.Start(ctx)
	a.quotaLoop.Start(ctx)
	a.alertLoop.Start(ctx)
	go a.passAuditLoop.Start(ctx)
	go a.authKeyLoop.Start(ctx)
	a.collector.Start(ctx)
}

// stopBackground 停止需要优雅收尾的周期组件。
//
// 只停这三个：与拆分前 main 里的三个 defer 一一对应。passAuditLoop /
// authKeyLoop / scheduler 原本就没有 Stop（进程退出即止），这里不新增。
func (a *app) stopBackground() {
	a.collector.Stop()
	a.alertLoop.Stop()
	a.quotaLoop.Stop()
}

// newHTTPServer 创建 Hertz 服务实例。
func (a *app) newHTTPServer() *server.Hertz {
	// Hertz 默认只收 4 MiB 的请求体，而分片上传的**每一片**是 8 MiB
	// （见 web/src/api/userstorage.ts 的 CHUNK_SIZE）。不放开这个上限，
	// 每一片都会在服务端被直接拒掉（413），界面只能显示一句「上传失败」——
	// 而小于 4 MiB 的文件照样能传，于是这看起来像是"偶发"。
	// 留一倍余量：调大前端分片时不必同时改这里。
	return server.Default(
		server.WithHostPorts(a.cfg.HTTP.Addr()),
		server.WithMaxRequestBodySize(16<<20),
	)
}

// registerRoutes 注册全部路由。
func (a *app) registerRoutes(h *server.Hertz) {
	router.Register(h, a.routerDeps())
}

// routerDeps 汇集路由注册所需的全部依赖。
func (a *app) routerDeps() router.Deps {
	return router.Deps{
		DB:            a.db,
		Auth:          a.authSvc,
		Bootstrap:     a.bootstrap,
		Node:          a.nodeSvc,
		VM:            a.vmSvc,
		Task:          a.queue,
		Risk:          a.riskGuard,
		Schedule:      a.scheduleSvc,
		Template:      a.templateSvc,
		Quota:         a.quotaSvc,
		Importer:      a.importerSvc,
		PublicIP:      a.publicIPSvc,
		SecurityGroup: a.sgSvc,
		UserStorage:   a.userStorageSvc,
		Scheduler:     a.schedulerSvc,
		PortSecurity:  a.portSecuritySvc,
		Capture:       a.captureSvc,
		QuotaEnforce:  a.quotaEnforceSvc,
		HostFirewall:  a.hostFirewallSvc,
		Passthrough:   passthrough.NewService(a.db, a.queue, a.mockAgent, a.recorder),
		HostTuning:    a.hostTuningSvc,
		PlatformCheck: platformcheck.NewService(a.db, a.queue, a.mockAgent, a.recorder),
		AccessControl: accesscontrol.NewService(a.db, a.recorder, accesscontrol.Options{}),
		Diagnostics:   a.diagnosticsSvc,
		Firewall:      a.firewallSvc,
		APIKey:        a.apiKeySvc,
		PortMirror:    a.mirrorSvc,
		NetworkBridge: a.netSvc,
		AuditLog:      a.auditLogSvc,
		AuditRecorder: a.recorder,
		Logging:       a.logger,
		ReqLog:        a.reqLogSvc,
		PassAudit:     a.passAuditSvc,
		AuthKey:       a.authKeySvc,
		Invite:        a.inviteSvc,
		UserAdmin:     a.userAdminSvc,
		VMTag:         a.tagSvc,
		Monitor:       a.monitorSvc,
		Dashboard:     a.dashboardSvc,
		ComputeQuota:  a.computeQuotaSvc,
		Search:        search.NewService(a.db),
		Alert:         a.alertSvc,
		Storage:       a.storageSvc,
		Network:       a.networkSvc,
		Settings:      a.settingsSvc,
		Mailer:        a.mailSvc,
		Bus:           a.bus,
		VpcACL:        vpcacl.NewService(a.db, a.mockAgent, a.queue, a.recorder),
		SecureCookie:  a.cfg.Session.SecureCookie,
		SimulateAgent: a.cfg.Agent.Transport == config.AgentTransportMock,
	}
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
