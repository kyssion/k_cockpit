// Package main 是 HTTP 服务的入口。
//
// 启动流程：加载配置 -> 连接数据库 -> 装配依赖 -> 注册路由 -> 启动服务。
// 表结构由 internal/platform/database/migrations/ 下的 SQL 迁移管理（见
// docs/02-architecture/DATA_MODEL.md 第 6 节），启动流程不做自动迁移。
//
// 装配代码按 internal/ 的域拆在 wire_*.go 里（platform / node / task_queue /
// storage / network / compute / ops / background）；本文件只剩生命周期骨架：
// app 结构、装配顺序总控（setupServices）与 HTTP / 路由映射。
package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/joho/godotenv"
	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/ops/realtime"
	sched "k_cockpit/internal/ops/scheduler"
	"k_cockpit/internal/ops/task"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/platform/logging"
	"k_cockpit/internal/platform/useradmin"
	"k_cockpit/internal/router"
	"k_cockpit/internal/storage/quota"
)

// app 汇集启动期装配好的全部依赖。
//
// main 曾是一个 450 行的函数，装配顺序与跨服务接线全挤在一起，读它要一次性
// 记住五十多个局部变量。用一个结构体承载这些依赖、按模块拆成若干 setup* 方法
// 之后，main 只剩「按序装配 -> 启动 -> 注册路由」的骨架，各模块内部的接线细节
// 收敛到对应方法里。
//
// 字段按两层组织，与 internal/ 的目录结构对应：
//
//   - **基座**：被全部域共享的实例（数据库、agent 通道、任务队列、审计记录器），
//     挂在 app 根上——塞进任何子结构都会让接线写成 a.xxx.queue 这样更长的路径；
//   - **领域**：与 internal/ 同名的六个子结构（platform / node / compute /
//     network / storage / ops），各自的装配代码在 wire_<域>.go；周期组件天然
//     跨域，按生命周期单独归入 bg。
//
// 方法之间的**调用顺序就是装配顺序**，其中若干处有硬约束（日志最先、总线先于
// 队列、调度记录器先于所有周期组件、加密密钥在受理任何任务前注入）。这些约束
// 原本靠"变量在文件里的先后位置"隐式表达，现在集中在 main 的调用序列里，一眼
// 可见；跨方法的依赖通过结构体字段传递，因此每个 setup* 只依赖在它之前调用过
// 的方法已经填好的字段。
type app struct {
	// --- 基座（跨域共享；括号内为对应的 internal 包）---

	cfg    config.Config   // internal/platform/config
	logger *logging.Logger // internal/platform/logging
	db     *gorm.DB        // internal/platform/database
	// mockAgent 是节点通道（internal/agent）的当前唯一实现，全部域共用；
	// 真实 agent 落地后换类型，装配代码不感知（ADR-0007）。
	mockAgent *agent.MockClient
	// recorder 是审计记录器（internal/platform/audit），全部写操作共用。
	recorder *audit.Recorder
	// bus / queue 是实时事件总线与任务队列（internal/ops/realtime、task），
	// 全部领域执行器的载体。
	bus   *realtime.Bus
	queue *task.Queue
	// schedRegistry / schedRecorder 是调度器注册表与记录器
	// （internal/ops/scheduler），必须先于所有周期组件与调度视图构造。
	schedRegistry *sched.Registry
	schedRecorder *sched.Recorder

	// --- 领域（对应 internal/ 同名目录，装配代码在 wire_<域>.go）---

	platform platformServices // internal/platform：认证、设置、用户与邀请
	node     nodeServices     // internal/node：节点接入与投影
	storage  storageServices  // internal/storage：池、用户存储、存储配额
	network  networkServices  // internal/network：交换机、防火墙、公网 IP 等
	compute  computeServices  // internal/compute：虚拟机、模板、导入、直通
	ops      opsServices      // internal/ops：监控、工作台、告警、维护等

	// bg 是后台周期组件：跨域，按生命周期归组（wire_background.go）。
	bg backgroundLoops
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
	a.seedDevData()
	a.hydrateMockPowerState()
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

// setupServices 按域装配全部业务服务并完成它们之间的接线。
//
// 调用顺序是依赖序，不是任意的排列：
//
//	platform（设置）→ storage（配额）→ network → compute → platform（用户簇）
//	→ ops（聚合一切）→ 跨域接线
//
// 其中两处顺序值得说明：storage 的配额被 compute（模板/导入）与 platform
// （用户管理的配额初始化）依赖，因此靠前；platform 拆成两步是因为用户管理
// 要接 storage 的配额写入，认证簇却必须在一切之前（setupAuth）。
func (a *app) setupServices() {
	a.setupPlatformServices()
	a.setupStorageServices()
	a.setupNetworkServices()
	a.setupComputeServices()
	a.setupPlatformAdminServices()
	a.setupOpsServices()
	a.setupCrossWiring()
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
//
// 只做「字段到字段」的映射：所有服务在各域的 wire 文件里构造完毕，
// 这里不再出现 New*。
func (a *app) routerDeps() router.Deps {
	return router.Deps{
		DB:            a.db,
		Auth:          a.platform.authSvc,
		Bootstrap:     a.platform.bootstrap,
		Node:          a.node.nodeSvc,
		VM:            a.compute.vmSvc,
		Task:          a.queue,
		Risk:          a.platform.riskGuard,
		Schedule:      a.ops.scheduleSvc,
		Template:      a.compute.template,
		Quota:         a.storage.quota,
		Importer:      a.compute.importer,
		PublicIP:      a.network.publicIP,
		SecurityGroup: a.network.sg,
		UserStorage:   a.storage.userStorage,
		Scheduler:     a.ops.schedulerSvc,
		PortSecurity:  a.network.portSecurity,
		Capture:       a.network.capture,
		QuotaEnforce:  a.ops.quotaEnforce,
		HostFirewall:  a.network.hostFirewall,
		Passthrough:   a.compute.passthrough,
		HostTuning:    a.ops.hostTuning,
		PlatformCheck: a.ops.platformChk,
		AccessControl: a.platform.accessControl,
		Diagnostics:   a.ops.diagnostics,
		Firewall:      a.network.firewall,
		APIKey:        a.platform.apiKey,
		PortMirror:    a.network.mirror,
		NetworkBridge: a.network.netSvc,
		AuditLog:      a.platform.auditLog,
		AuditRecorder: a.recorder,
		Logging:       a.logger,
		ReqLog:        a.platform.reqLog,
		PassAudit:     a.platform.passAudit,
		AuthKey:       a.platform.authKey,
		Invite:        a.platform.invite,
		UserAdmin:     a.platform.userAdmin,
		VMTag:         a.compute.tag,
		Monitor:       a.ops.monitor,
		Dashboard:     a.ops.dashboard,
		ComputeQuota:  a.compute.computeQuota,
		Search:        a.ops.search,
		Alert:         a.ops.alert,
		Maintenance:   a.ops.maintenance,
		Storage:       a.storage.storageSvc,
		Network:       a.network.networkSvc,
		Settings:      a.platform.settings,
		Mailer:        a.platform.mailer,
		Bus:           a.bus,
		VpcACL:        a.network.vpcACL,
		SecureCookie:  a.cfg.Session.SecureCookie,
		SimulateAgent: a.cfg.Agent.Transport == config.AgentTransportMock,
		// 请求过滤开关每次请求现读设置：改设置立即生效，不需要重启。
		InputFilterEnabled: func() bool {
			return a.platform.settings.Bool("security.request_filter_enabled", true)
		},
	}
}

// notifyUserEmail 把系统通知发给单个用户（配额处置 / 口令检查命中）。
//
// 只发**已验证**邮箱：发到没验证过的地址等于把面板存在的信息投给一个
// 未必属于该用户的信箱。未绑定邮箱、SMTP 未配置都只记日志——通知是
// 附属品，缺了它处置与检查照常生效。
func (a *app) notifyUserEmail(ctx context.Context, userID int64, subject, body string) {
	var user model.User
	if err := a.db.WithContext(ctx).Select("email", "email_verified_at").
		First(&user, userID).Error; err != nil || user.Email == nil || user.EmailVerifiedAt == nil {
		return
	}
	if a.platform.settings.String("notification.smtp_host", "") == "" {
		log.Printf("[notify] 用户 %d 有已验证邮箱但 SMTP 未配置，通知未发出", userID)
		return
	}
	if err := a.platform.mailer.Send(ctx, *user.Email, subject, body); err != nil {
		log.Printf("[notify] 通知邮件发送失败 user=%d: %v", userID, err)
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

// intApplier 把「设置项改成整数并立即应用」接成一个 settings.Applier。
//
// 只做解析与回调：具体的生效动作（如调整日志保留数）由被装配方自己保证
// 立即生效，失败返回错误以触发设置框架的回滚（R-007）。
type intApplier struct{ fn func(int) }

func (a intApplier) Apply(_ context.Context, _ string, value string) error {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("不是合法整数: %q", value)
	}
	a.fn(n)
	return nil
}
