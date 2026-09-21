// Package router 集中注册 HTTP 路由。
//
// 所有对外暴露的接口都在这里登记，便于一眼看清服务的 API 面，也便于
// 集中审查「哪些接口需要什么角色」（f-1-06 R-003：角色要求在这里声明，
// 而不是散落在各 handler 内部判断）。
//
// 业务接口随功能实现逐步接入，并在 docs/03-api/API.md 的接口清单中登记。
package router

import (
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"k_cockpit/internal/accesscontrol"
	"k_cockpit/internal/alert"
	"k_cockpit/internal/api"
	"k_cockpit/internal/apikey"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auditlog"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/capture"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/dashboard"
	"k_cockpit/internal/diagnostics"
	"k_cockpit/internal/firewall"
	"k_cockpit/internal/handler"
	"k_cockpit/internal/hostfirewall"
	"k_cockpit/internal/hosttuning"
	"k_cockpit/internal/importer"
	"k_cockpit/internal/logging"
	"k_cockpit/internal/mailer"
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
	"k_cockpit/internal/realtime"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/schedule"
	"k_cockpit/internal/scheduler"
	"k_cockpit/internal/search"
	"k_cockpit/internal/securitygroup"
	"k_cockpit/internal/settings"
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
	"k_cockpit/internal/template"
	"k_cockpit/internal/useradmin"
	"k_cockpit/internal/userstorage"
	"k_cockpit/internal/vm"
	"k_cockpit/internal/vmtag"
	"k_cockpit/internal/vpcacl"
)

// Deps 是路由注册所需的外部依赖。
//
// 通过参数传入而非包级变量：路由与 handler 的依赖关系一目了然，
// 测试也能用替身构造独立的引擎。
type Deps struct {
	DB        *gorm.DB
	Auth      *auth.Service
	Bootstrap *auth.Bootstrap
	Node      *node.Service
	VM        *vm.Service
	Storage   *storage.Service
	Network   *network.Service
	Settings  *settings.Service
	Task      *task.Queue
	// Mailer 提供发信能力（F-1-08）。为 nil 时测试发信接口返回不可用。
	Mailer *mailer.Service
	// Bus 是实时事件总线（F-7-02 的任务流）。为 nil 时实时通道返回"未启用"。
	Bus *realtime.Bus
	// VpcACL 提供 VPC 网络的访问控制（F-4-05）。为 nil 时 ACL 接口不可用。
	VpcACL *vpcacl.Service
	// Risk 强制高风险操作的二次验证（f-10-01）。受保护的操作在 handler
	// 入口调用它，清单本身集中在 internal/risk。
	Risk *risk.Guard
	// Schedule 提供虚拟机的定时任务（F-7-05）。
	Schedule *schedule.Service
	// Template 提供模板管理与模板克隆（F-3-01 / F-3-02）。
	Template *template.Service
	// Quota 提供按用户按节点的存储配额（F-9-02）。
	Quota *quota.Service
	// Importer 提供磁盘与镜像导入（F-2-13）。
	Importer *importer.Service
	// PublicIP 提供公网地址池、绑定与浮动迁移（F-4-06）。
	PublicIP *publicip.Service
	// SecurityGroup 提供安全组与叠加生效（F-4-03 / F-4-04）。
	SecurityGroup *securitygroup.Service
	// UserStorage 提供用户存储空间与分片上传（F-5-03/04/05）。
	UserStorage *userstorage.Service
	// Scheduler 提供周期性调度器与调度事件查询（F-7-04）。
	Scheduler *scheduler.Service
	// PortSecurity 提供端口安全（F-4-08）。
	PortSecurity *portsecurity.Service
	// Capture 提供抓包与网络诊断（F-4-12）。
	Capture *capture.Service
	// Diagnostics 提供诊断导出（F-9-03）。
	Diagnostics *diagnostics.Service
	// Firewall 提供双层防火墙策略（F-4-11）。
	Firewall *firewall.Service
	// APIKey 提供 API 凭证与一次性令牌（F-1-10）。
	APIKey *apikey.Service
	// PortMirror 提供端口镜像与自动撤销看门狗（F-4-09）。
	PortMirror *portmirror.Service
	// NetworkBridge 提供网络底座状态与自愈（F-4-01 / F-4-13）。
	NetworkBridge *networkbridge.Service
	// AuditLog 提供审计流水查询（F-1-12）。
	AuditLog *auditlog.Service
	// Logging 提供服务端日志的级别、查看、导出与清理（F-9-02）。
	Logging *logging.Logger
	// QuotaEnforce 提供资源配额与超限处置（F-4-10）。
	QuotaEnforce *quotaenforce.Service
	// HostFirewall 提供宿主机防火墙（F-4-11 第一层）。
	HostFirewall *hostfirewall.Service
	// Passthrough 提供 PCIe 直通。
	Passthrough *passthrough.Service
	// HostTuning 提供宿主机性能调优。
	HostTuning *hosttuning.Service
	// PlatformCheck 提供平台自检与修复（F-4-13）。
	PlatformCheck *platformcheck.Service
	// AccessControl 提供公网访问与开发模式开关（F-10-06）。
	AccessControl *accesscontrol.Service
	// AuditRecorder 供**跨服务**的写操作记审计用。
	//
	// 多数 handler 不需要它：那个包的 service 自己持有记录器。但账号自管理
	// 横跨 auth（改凭据）与 risk（一次性许可）两个服务，而"改密码"这件事
	// 本身不属于它们中任何一个——记在调用方最直接。
	AuditRecorder *audit.Recorder
	// UserAdmin 提供用户管理（F-1-07）。
	UserAdmin *useradmin.Service
	// VMTag 提供虚拟机标签（F-2-16）。
	VMTag *vmtag.Service
	// Monitor 提供指标历史查询（F-8-01 / F-8-02）。
	Monitor *monitor.Service
	// Dashboard 提供工作台概览（F-8-03 / F-8-04）。
	Dashboard *dashboard.Service
	// ComputeQuota 提供计算资源配额（vCPU / 内存 / 实例数）。
	ComputeQuota *computequota.Service
	// Search 提供跨资源检索（F-9-08）。
	Search *search.Service
	// Alert 提供告警中心（F-8-07）。
	Alert *alert.Service

	SecureCookie bool
	// SimulateAgent 为 true 时注册开发期的模拟注册入口。
	// 仅在 AGENT_TRANSPORT=mock 时开启；该开关为 mock 专用。
	SimulateAgent bool
}

// Register 注册全局中间件与全部路由。
func Register(h *server.Hertz, deps Deps) {
	// 顺序：RequestID 最先（后续都要用它）；Recover 包裹业务处理，
	// 保证 panic 也被记录 request_id；AccessLog 在最内层以准确统计耗时。
	h.Use(api.RequestID(), api.Recover(), api.AccessLog())

	h.GET("/health", handler.Health(deps.DB))

	// 登录阶段的二次验证复用 risk 的校验逻辑（恢复码一次性、TOTP 容差与
	// 算法参数只有一处定义）。接线放在这里而不是启动脚本里：漏接的表现是
	// 「登录时永远提示服务不可用」，而它不会在编译期暴露。
	if deps.Auth != nil && deps.Risk != nil {
		deps.Auth.SetLoginVerifier(deps.Risk)
	}

	authHandler := handler.NewAuth(deps.Auth, deps.SecureCookie, deps.Risk)
	setupHandler := handler.NewSetup(deps.Bootstrap, deps.Auth, deps.SecureCookie)
	nodeHandler := handler.NewNode(deps.Node, deps.SimulateAgent, deps.Risk)
	vmHandler := handler.NewVM(deps.VM, deps.Risk)
	taskHandler := handler.NewTask(deps.Task, deps.Bus)
	securityHandler := handler.NewSecurity(deps.Risk, deps.Auth)
	accountHandler := handler.NewAccount(deps.Auth, deps.Risk, deps.AuditRecorder)
	scheduleHandler := handler.NewSchedule(deps.Schedule, deps.Risk)
	storageHandler := handler.NewStorage(deps.Storage, deps.Risk)
	networkHandler := handler.NewNetwork(deps.Network)
	vpcACLHandler := handler.NewVpcACL(deps.VpcACL)
	settingsHandler := handler.NewSettings(deps.Settings, deps.Mailer)
	consoleHandler := handler.NewConsole(deps.VM, deps.Risk)
	templateHandler := handler.NewTemplate(deps.Template)
	quotaHandler := handler.NewQuota(deps.Quota)
	publicIPHandler := handler.NewPublicIP(deps.PublicIP)
	sgHandler := handler.NewSecurityGroup(deps.SecurityGroup)
	userStorageHandler := handler.NewUserStorage(deps.UserStorage)
	volumeHandler := handler.NewStorageVolume(deps.Storage)
	schedulerHandler := handler.NewScheduler(deps.Scheduler)
	portSecurityHandler := handler.NewPortSecurity(deps.PortSecurity)
	captureHandler := handler.NewCapture(deps.Capture)
	diagnosticsHandler := handler.NewDiagnostics(deps.Diagnostics)
	versionHandler := handler.NewVersion()
	loggingHandler := handler.NewLogging(deps.Logging, deps.AuditRecorder)
	quotaEnforceHandler := handler.NewQuotaEnforce(deps.QuotaEnforce)
	hostFirewallHandler := handler.NewHostFirewall(deps.HostFirewall)
	passthroughHandler := handler.NewPassthrough(deps.Passthrough)
	tuningHandler := handler.NewHostTuning(deps.HostTuning)
	platformCheckHandler := handler.NewPlatformCheck(deps.PlatformCheck)
	accessControlHandler := handler.NewAccessControl(deps.AccessControl)
	firewallHandler := handler.NewFirewall(deps.Firewall)
	apiKeyHandler := handler.NewAPIKey(deps.APIKey)
	mirrorHandler := handler.NewPortMirror(deps.PortMirror)
	netHandler := handler.NewNetworkBridge(deps.NetworkBridge)
	auditHandler := handler.NewAuditLog(deps.AuditLog)
	userAdminHandler := handler.NewUserAdmin(deps.UserAdmin)
	tagHandler := handler.NewVMTag(deps.VMTag)
	monitorHandler := handler.NewMonitor(deps.Monitor)
	dashboardHandler := handler.NewDashboard(deps.Dashboard)
	// 接口清单取自**已注册的路由**，因此必须在全部路由注册完成之后构造。
	apiDocsHandler := handler.NewAPIDocs(h)
	computeQuotaHandler := handler.NewComputeQuota(deps.ComputeQuota)
	searchHandler := handler.NewSearch(deps.Search)
	alertHandler := handler.NewAlert(deps.Alert)
	importerHandler := handler.NewImporter(deps.Importer)

	// 挂上 API 凭证认证：客户端可用 `Authorization: Bearer kc_...` 代替会话 Cookie。
	//
	// 注销、改密码这类会话管理接口在凭证认证下会拿到 nil 会话，它们的既有
	// 判空逻辑因此会正确地拒绝——不会出现「用 API Key 把自己登出」这种操作。
	authMW := auth.NewMiddleware(deps.Auth).WithAPIKey(deps.APIKey)
	// 认证接口都计为真实用户活动：它们由用户显式操作触发，不是后台轮询。
	requireAuth := authMW.Require(auth.Real)
	adminOnly := authz.Admin()

	v1 := h.Group("/api/v1")
	{
		// 公开接口：系统尚无管理员时不可能要求认证（自举问题，见 ADR-0008）。
		// 安全性由「一次性令牌只能从服务端日志获取」保证。
		v1.GET("/setup/status", setupHandler.Status)
		v1.POST("/setup/admin", setupHandler.CreateAdmin)

		// 公开接口：获取凭据的入口，必须在 API.md 中显式标记为公开。
		v1.POST("/auth/login", authHandler.Login)

		// 登录后续阶段（F-1-08）。
		//
		// **不挂 requireAuth**：它们用请求体里的 login_token 认证，而那个
		// 令牌是中间态（五分钟、只差一步到完整权限），走 Cookie 会话中间
		// 件会把它当成完整会话放过去。
		v1.POST("/auth/login/verify", authHandler.VerifyLogin)
		v1.POST("/auth/login/password", authHandler.ForceChangePassword)
		v1.POST("/auth/bootstrap/skip", authHandler.SkipBootstrap)
		v1.POST("/auth/bootstrap/totp/setup", authHandler.StagedBeginTOTP)
		v1.POST("/auth/bootstrap/totp/confirm", authHandler.StagedConfirmTOTP)
		v1.POST("/auth/bootstrap/email/code", authHandler.StagedSendEmailCode)
		v1.POST("/auth/bootstrap/email/confirm", authHandler.StagedConfirmEmail)

		// 找回密码（公开）。三步而不是一步：邮件里的码只用来换一张短命的
		// 重置票据，能改密码的凭据因此不经过邮箱。
		v1.POST("/auth/forgot/send", authHandler.RequestPasswordReset)
		v1.POST("/auth/forgot/verify", authHandler.VerifyResetCode)
		v1.POST("/auth/forgot/reset", authHandler.ResetPassword)

		v1.POST("/auth/logout", requireAuth, authHandler.Logout)
		v1.GET("/auth/session", requireAuth, authHandler.Session)
		v1.GET("/auth/sessions", requireAuth, authHandler.Sessions)
		v1.DELETE("/auth/sessions/:id", requireAuth, authHandler.RevokeSession)

		// 高风险二次验证（f-10-01）：清单只读，验证接口换取一次性许可。
		// 清单是**唯一事实来源**，前端不得硬编码第二份（R-002）。
		v1.GET("/security/high-risk-policy", requireAuth, securityHandler.Policy)
		v1.POST("/auth/risk-verification", requireAuth, securityHandler.Verify)

		// 账号自管理（F-1-03 / F-10-01）。
		//
		// 三件事都要提供**当前密码**。这不是形式主义：拿到一个未锁屏的浏览器
		// 或偷到一个会话令牌就能改掉密码并把主人锁在外面，而那时主人连
		// "怎么进不去了"都查不出来——会话是合法的，日志里看不出异常。
		//
		// **刻意不要求二次验证**：用户重新生成恢复码的常见原因恰恰是"手机
		// 丢了、恢复码快用完了"，那时他刚用掉一个恢复码登进来。要求 TOTP
		// 就是要求他拿出已经丢了的东西，那条路会彻底走不通。
		// 邮箱绑定（F-1-08）。验证码发往待绑定地址，因此**不需要**该邮箱
		// 当前属于自己——否则改绑就走不通了。
		v1.POST("/auth/email/code", requireAuth, authHandler.SendEmailCode)
		v1.PUT("/auth/email", requireAuth, authHandler.ConfirmEmail)

		v1.PUT("/auth/password", requireAuth, accountHandler.ChangePassword)
		v1.PUT("/auth/username", requireAuth, accountHandler.ChangeUsername)
		v1.POST("/auth/recovery-codes", requireAuth, accountHandler.RegenerateRecoveryCodes)
		// 补齐安全设置后清除"已跳过引导"标记（F-1-08）。走访问级会话：
		// 邮箱与 2FA 都是在安全中心补齐的，那时登录早已完成。
		v1.POST("/auth/bootstrap/complete", requireAuth, authHandler.CompleteBootstrap)

		// 二次验证方式的绑定：没有绑定渠道，428 将永远无法通过。
		v1.GET("/auth/security-setup", requireAuth, securityHandler.SetupStatus)
		v1.POST("/auth/totp/setup", requireAuth, securityHandler.BeginTOTP)
		v1.POST("/auth/totp/confirm", requireAuth, securityHandler.ConfirmTOTP)

		// 节点管理：按 f-1-06 的角色表，全部仅管理员可访问。
		v1.GET("/nodes", requireAuth, adminOnly, nodeHandler.List)
		v1.GET("/nodes/:id", requireAuth, adminOnly, nodeHandler.Get)
		// 宿主机指标（F-6-03）。只读探测，不入队、不写投影——指标是瞬时的，
		// 存下来只会在下一次读取时给出一个过期的答案。
		v1.GET("/nodes/:id/stats", requireAuth, adminOnly, nodeHandler.Stats)

		// 维护模式（F-6-05 / API-042）。**同步生效，不进任务队列**——
		// 它纯粹是控制面的标志，所有拦截都发生在受理那一刻；做成任务会
		// 制造一个「界面说维护中、操作仍被受理」的窗口。
		v1.PATCH("/nodes/:id/maintenance", requireAuth, adminOnly, nodeHandler.SetMaintenance)
		// 控制台对外地址：决定能否生成 SPICE 连接文件。
		v1.PATCH("/nodes/:id/console-host", requireAuth, adminOnly, nodeHandler.SetConsoleHost)

		v1.POST("/nodes/registration-tokens", requireAuth, adminOnly, nodeHandler.CreateEnrollToken)
		v1.DELETE("/nodes/:id", requireAuth, adminOnly, nodeHandler.Remove)

		// 存储池（F-5-01）：管理员专属。创建与删除会格式化/销毁设备，
		// 属受二次验证保护的操作（在 handler 入口调用 guard）。
		v1.GET("/nodes/:id/disks", requireAuth, adminOnly, storageHandler.Disks)
		v1.GET("/nodes/:id/storage-pools", requireAuth, adminOnly, storageHandler.ListPools)
		v1.GET("/storage-pools/:id", requireAuth, adminOnly, storageHandler.GetPool)
		v1.POST("/storage-pools", requireAuth, adminOnly, storageHandler.CreatePool)
		v1.PATCH("/storage-pools/:id", requireAuth, adminOnly, storageHandler.UpdatePool)
		v1.DELETE("/storage-pools/:id", requireAuth, adminOnly, storageHandler.DeletePool)
		// 分区、池配置、卸载、trim（F-5-01 的后续迭代）。
		//
		// 池配置单独一个接口而不是扩展 PATCH：PATCH 是"只改控制面一列、
		// 同步返回"，而配置要下发到节点改挂载与 fstab——两者放在一起会让
		// "保存"在不同字段上有完全不同的耗时与失败语义。
		v1.GET("/storage-partitions", requireAuth, adminOnly, storageHandler.Partitions)
		v1.POST("/storage-partitions", requireAuth, adminOnly, storageHandler.CreatePartition)
		v1.DELETE("/storage-partitions", requireAuth, adminOnly, storageHandler.DeletePartitions)
		v1.POST("/storage-pools/:id/config", requireAuth, adminOnly, storageHandler.UpdatePoolConfig)
		v1.POST("/storage-pools/:id/unmount", requireAuth, adminOnly, storageHandler.UnmountPool)
		v1.POST("/storage/trim", requireAuth, adminOnly, storageHandler.TrimStorage)

		// 网络（F-4-01）：M2 只有只读接口——能力探测与降级说明。
		// 网络变更（建网桥、物理口入桥）属 M3 范围。
		// VPC ACL（F-4-05）：挂在**网段**上的访问控制，与安全组（挂在虚拟机
		// 网口上）并列但不同。必须先预览后应用，且应用要带回预览版本号。
		v1.GET("/vpc-acl", requireAuth, adminOnly, vpcACLHandler.List)
		v1.POST("/vpc-acl", requireAuth, adminOnly, vpcACLHandler.Create)
		v1.PUT("/vpc-acl/:id", requireAuth, adminOnly, vpcACLHandler.Update)
		v1.DELETE("/vpc-acl/:id", requireAuth, adminOnly, vpcACLHandler.Delete)
		v1.GET("/vpc-acl/preview", requireAuth, adminOnly, vpcACLHandler.Preview)
		v1.POST("/vpc-acl/apply", requireAuth, adminOnly, vpcACLHandler.Apply)

		v1.GET("/nodes/:id/network", requireAuth, adminOnly, networkHandler.Status)
		v1.GET("/nodes/:id/networks", requireAuth, adminOnly, networkHandler.Networks)

		// 虚拟交换机（F-4-02）。写操作走任务队列——建网桥是宿主机上的实际
		// 操作，接口不同步等待；记录由执行器在节点成功后写入。
		//
		// 不需要二次验证：交换机变更**可逆**（改回去即可），而它影响的是
		// 网络连通性而非数据。给可逆操作加验证只会稀释验证本身的分量。
		v1.POST("/nodes/:id/vpc-switches", requireAuth, adminOnly, networkHandler.CreateSwitch)
		v1.PATCH("/vpc-switches/:id", requireAuth, adminOnly, networkHandler.UpdateSwitch)
		// 迁移（换物理网卡 / VLAN）与重配置（按记录重新下发）。两者都走任务队列。
		v1.POST("/vpc-switches/:id/migrate", requireAuth, adminOnly, networkHandler.MigrateSwitch)
		v1.POST("/vpc-switches/:id/reconfigure", requireAuth, adminOnly, networkHandler.ReconfigureSwitch)
		// 端口释放（回收节点上的端口资源）、计数重置、IPv6 策略。
		v1.POST("/networks/ports/release", requireAuth, adminOnly, networkHandler.ReleasePort)
		v1.POST("/networks/counters/reset", requireAuth, adminOnly, networkHandler.ResetCounters)
		v1.POST("/networks/ipv6/policy", requireAuth, adminOnly, networkHandler.ApplyIPv6Policy)
		v1.DELETE("/vpc-switches/:id", requireAuth, adminOnly, networkHandler.DeleteSwitch)

		// 系统设置（F-9-01）：**仅管理员**（R-014）。设置变更不得成为
		// 绕过权限的通道，因此 tenant 连可见性都没有。
		v1.GET("/settings", requireAuth, adminOnly, settingsHandler.List)
		v1.PATCH("/settings", requireAuth, adminOnly, settingsHandler.Update)
		v1.POST("/settings/rollback", requireAuth, adminOnly, settingsHandler.Rollback)
		// 测试发信（F-1-08）：SMTP 配置是否正确，只有真的发一封才知道。
		v1.POST("/settings/mail/test", requireAuth, adminOnly, settingsHandler.TestMail)

		// 虚拟机：管理员可操作全部，tenant 仅自己名下（归属过滤在数据访问层注入，
		// 因此这里不需要按角色分路由）。
		v1.GET("/vms", requireAuth, vmHandler.List)
		v1.GET("/vms/:id", requireAuth, vmHandler.Get)
		// 创建表单元数据（F-2-02）：字段、取值、默认值与前置条件**由后端
		// 下发**，前端不维护第二份规则。放在 /vms/:id 之前注册，避免被
		// 通配路径吃掉。
		v1.GET("/vms/create-form", requireAuth, vmHandler.CreateForm)
		v1.POST("/vms", requireAuth, vmHandler.Create)
		// 批量操作（F-2-01）：电源与删除。逐台独立受理、可部分成功，
		// 因此始终返回 200，逐台的结果在响应体里。
		//
		// 删除动作在此处走一次二次验证（本文件是角色与验证要求的集中声明处）。
		v1.POST("/vms/batch-actions", requireAuth, vmHandler.BatchAction)

		// 业务软锁（F-2-12）。同步生效——锁只在控制面，虚拟化层不知道它。
		// 解锁需要二次验证，因此下面这条路由也受 risk 保护（在 handler 内声明）。
		v1.PATCH("/vms/:id/lock", requireAuth, vmHandler.SetLock)

		// 模板管理与模板克隆（F-3-01 / F-3-02）。
		//
		// 读接口对所有登录用户开放——可见性（已发布 / 自己创建的）由服务层
		// 过滤，不在路由层按角色一刀切：私有模板的所有者本来就应该能看到
		// 自己的东西，哪怕他只是 tenant。
		v1.GET("/templates", requireAuth, templateHandler.List)
		v1.GET("/templates/:id", requireAuth, templateHandler.Get)
		v1.POST("/templates", requireAuth, templateHandler.CreateFromVM)
		v1.PATCH("/templates/:id", requireAuth, templateHandler.Update)
		// 删除前的检查。**只读**，且与 Delete 共用同一段判定——两处各写一遍
		// 迟早分叉，而分叉的表现是「预览说可以删、点下去却报冲突」。
		v1.GET("/templates/:id/delete-preview", requireAuth, templateHandler.DeletePreview)
		// 模板族：同一条派生链上的全部版本（F-3-04）。放在 :id 通配之后也不会
		// 被吃掉，因为它多一段路径。
		v1.GET("/templates/:id/family", requireAuth, templateHandler.Family)

		// 模板导出与导入（F-3-05）：模板包是跨节点搬运模板的载体。
		//
		// 导出记在 /template-exports 下而不是 /templates/:id/exports：产物是
		// **独立的对象**（可以下载、可以删除、模板删了它还在），挂在模板下面
		// 会让"模板已删除"之后无处可找。
		v1.POST("/templates/:id/exports", requireAuth, templateHandler.Export)
		v1.GET("/template-exports", requireAuth, templateHandler.ListExports)
		v1.DELETE("/template-exports/:id", requireAuth, templateHandler.DeleteExport)
		v1.GET("/template-exports/:id/download", requireAuth, templateHandler.DownloadExport)
		v1.POST("/templates/imports/preview", requireAuth, templateHandler.ImportPreview)
		v1.POST("/templates/imports", requireAuth, templateHandler.Import)
		v1.DELETE("/templates/:id", requireAuth, templateHandler.Delete)

		// 跨节点迁移（F-2-09）。前置条件在受理时同步判定——每一个条件
		// 漏掉的代价都不是「操作失败」，而是两台宿主机上各留下一份不完整
		// 的东西。
		v1.POST("/vms/:id/migrate", requireAuth, vmHandler.Migrate)
		v1.GET("/vms/:id/migrations", requireAuth, vmHandler.Migrations)

		// 镜像导入（F-2-13）。
		//
		// **本面板不接收真实文件内容**：受理时只提交文件名、大小与格式
		// （都是浏览器能直接读到的元数据）。整条链路——受理、状态流转、
		// 转换后建模板、配额记账——都能被验证，唯独「字节怎么从浏览器到
		// 宿主机」这一段留空。那一段属于 mock 未覆盖的范围，是最难的部分之一。
		v1.GET("/imports/format", requireAuth, importerHandler.GuessFormat)
		v1.POST("/imports/parse", requireAuth, importerHandler.Parse)
		v1.GET("/imports", requireAuth, importerHandler.List)
		v1.POST("/imports", requireAuth, importerHandler.Create)
		v1.GET("/imports/:id", requireAuth, importerHandler.Get)
		v1.DELETE("/imports/:id", requireAuth, importerHandler.Delete)

		// 指标历史（F-8-01 / F-8-02）。
		//
		// 数据由**独立采集器按固定间隔写入**，不是「用户看页面时顺便采一次」
		// ——后者的密度由点击行为决定，用它算「过去一周的负载」会得到与真实
		// 情况毫无关系的结果，而它看起来像一份正常的图表。
		//
		// 默认时间范围是最近 1 小时：不给默认值的话，一次不带参数的调用会
		// 扫全表，而数据攒了几个月之后那会慢到让人以为接口挂了。
		v1.GET("/monitor/host", requireAuth, adminOnly, monitorHandler.HostSeries)
		// 可筛选的物理设备（网卡 / 磁盘）。清单来自最近一次采样而不是现探一次：
		// 设备名几乎不变，而为打开一个下拉框去节点上取一次不值得。
		v1.GET("/monitor/host/devices", requireAuth, adminOnly, monitorHandler.HostDevices)
		v1.GET("/vms/:id/monitor", requireAuth, monitorHandler.VMSeries)
		v1.GET("/vms/:id/runtime", requireAuth, monitorHandler.Runtime)

		// 工作台概览（F-8-03 / F-8-04）。
		//
		// **不要求管理员**：租户也有自己的工作台。范围由服务层按视角收敛，
		// 而不是靠路由层切断——后者的结果是租户打开首页就 403，而首页是
		// 登录后第一个到达的页面。
		v1.GET("/dashboard/summary", requireAuth, dashboardHandler.Summary)
		// 宿主机细节（调优 / 硬件 / 网络统计）。它需要**按节点**向节点发请求，
		// 因此不并进 Summary——否则首页会变成一次探测风暴。
		v1.GET("/dashboard/host-detail", requireAuth, dashboardHandler.HostDetail)

		// 虚拟机标签（F-2-16）。
		//
		// 标签与分组解决的不是同一件事：分组互斥（一台机器只属于一个组），
		// 标签非互斥（可以有任意多个）。合并会立刻遇到矛盾——一台机器既是
		// 「生产」又是「数据库」，而分组只能放一个。
		//
		// 保存是**整体替换**而不是增量增删：界面上改完直接保存，而 add/remove
		// 会让「加了又删、删了又加」的中间态被如实写进审计流水。
		v1.GET("/tags", requireAuth, tagHandler.All)
		v1.GET("/tags/vms", requireAuth, tagHandler.VMsByTag)
		v1.GET("/vms/:id/tags", requireAuth, tagHandler.List)
		v1.PUT("/vms/:id/tags", requireAuth, tagHandler.Set)

		// 用户管理（F-1-07）。整体归管理员。
		//
		// 封禁是一个**级联动作**（置状态 → 撤销会话 → 停运行中的虚拟机），
		// 且**单步失败仅告警**：封禁的实质是置状态，那一步就达成了目的；
		// 后两步是减少暴露面的加固，它们的失败不该把整个操作判为失败——
		// 那会让界面显示「封禁失败」，而那人其实已经被封了。
		//
		// 几处不可逆的自锁被显式挡住：不能改自己的角色、不能封禁自己、
		// 不能把最后一个可用管理员降级或删除。
		v1.GET("/users", requireAuth, adminOnly, userAdminHandler.List)
		v1.POST("/users", requireAuth, adminOnly, userAdminHandler.Create)
		v1.PATCH("/users/:id", requireAuth, adminOnly, userAdminHandler.Update)
		v1.PUT("/users/:id/status", requireAuth, adminOnly, userAdminHandler.SetStatus)
		v1.DELETE("/users/:id", requireAuth, adminOnly, userAdminHandler.Delete)
		// SSH 访问（F-1-10）。单独一个接口而不是塞进 PATCH：它会**结束在线会话**，
		// 与"改资料"不是一个量级的动作。
		v1.PUT("/users/:id/ssh", requireAuth, adminOnly, userAdminHandler.SetSSHAccess)

		// 审计流水（F-1-12）。
		//
		// 在此之前审计只有写入没有读取——Recorder 一直在记，但没有人能查。
		// **权限隔离在服务层**：租户只看得到自己的记录，且看不到系统动作
		// （operator_id 为空）。返回别人的记录时用 404 而非 403——403 会
		// 确认「这个 id 存在」。
		v1.GET("/audit", requireAuth, auditHandler.List)
		v1.GET("/audit/facets", requireAuth, auditHandler.Facets)
		v1.GET("/audit/:id", requireAuth, auditHandler.Get)

		// 网络底座与自愈（F-4-01 / F-4-13）。
		//
		// Overview 与 Repair **几乎不会失败**：探测不到节点、桥列表读不出来，
		// 都以字段形式返回，整体仍是 200。这是规格里「网络配置失败不得阻断
		// 主流程」的具体实现——用户点进这个页面本来就是为了看网络出了什么
		// 问题，给他白屏等于把唯一的诊断入口也关掉了。
		v1.GET("/networks", requireAuth, adminOnly, netHandler.Overview)
		v1.GET("/networks/bridges", requireAuth, adminOnly, netHandler.List)
		v1.POST("/networks/bridges", requireAuth, adminOnly, netHandler.CreateBridge)
		v1.DELETE("/networks/bridges/:id", requireAuth, adminOnly, netHandler.DeleteBridge)
		v1.POST("/networks/bridges/:id/uplink", requireAuth, adminOnly, netHandler.AttachUplink)
		v1.POST("/networks/bridges/:id/uplink/confirm", requireAuth, adminOnly, netHandler.ConfirmUplink)
		// 摘出入口不设门槛：一个已经切断管理通道的桥要能一键摘掉。
		v1.DELETE("/networks/bridges/:id/uplink", requireAuth, adminOnly, netHandler.DetachUplink)
		v1.POST("/networks/repair", requireAuth, adminOnly, netHandler.Repair)

		// 端口镜像（F-4-09）。
		//
		// **看门狗与镜像在同一次下发里建立**，且由**节点**计时——控制面是
		// 通过网络下发指令的，而端口镜像配错恰恰会打垮网络，那时控制面自己
		// 也发不出回滚指令。一个依赖网络的保险，在它最需要起作用的时候
		// 一定不在。
		v1.GET("/port-mirrors", requireAuth, adminOnly, mirrorHandler.List)
		v1.POST("/port-mirrors", requireAuth, adminOnly, mirrorHandler.Create)
		v1.PATCH("/port-mirrors/:id", requireAuth, adminOnly, mirrorHandler.Update)
		v1.DELETE("/port-mirrors/:id", requireAuth, adminOnly, mirrorHandler.Delete)
		v1.GET("/port-mirrors/:id/precheck", requireAuth, adminOnly, mirrorHandler.Precheck)
		v1.POST("/port-mirrors/:id/enable", requireAuth, adminOnly, mirrorHandler.Enable)
		v1.POST("/port-mirrors/:id/confirm", requireAuth, adminOnly, mirrorHandler.Confirm)
		// 关闭不设门槛。见 handler 上的说明。
		v1.POST("/port-mirrors/:id/disable", requireAuth, adminOnly, mirrorHandler.Disable)

		// API 凭证（F-1-10）与一次性动作令牌。
		//
		// 生成与撤销**只接受会话认证**（handler 内校验）：不允许用一个
		// API Key 去轮换 API Key——那会形成一个自我延续的凭据链，原持有者
		// 撤销它时攻击者手上那个仍然有效。
		v1.GET("/api-keys", requireAuth, apiKeyHandler.Get)
		v1.POST("/api-keys", requireAuth, apiKeyHandler.Create)
		v1.DELETE("/api-keys", requireAuth, apiKeyHandler.Revoke)
		v1.POST("/action-tokens", requireAuth, apiKeyHandler.IssueActionToken)

		// 防火墙（F-4-11）。双层：节点级基线 + 虚拟机级覆盖。
		//
		// 整体归 admin：它作用于宿主机，一次配错会影响该节点上所有虚拟机，
		// 而且**可能把管理员自己关在门外**——那不是租户该有的能力。
		//
		// 应用与回滚都是**同步调用节点、不经队列**（见 firewall.Service.Apply）：
		// 管理员点下按钮后立刻需要一个答复，而回滚更不能排在几十个
		// 虚拟机创建之后——那时他已经连不上面板了。
		v1.GET("/firewall/policy", requireAuth, adminOnly, firewallHandler.GetPolicy)
		v1.PATCH("/firewall/policy", requireAuth, adminOnly, firewallHandler.UpdatePolicy)
		v1.GET("/firewall/rules", requireAuth, adminOnly, firewallHandler.ListRules)
		v1.POST("/firewall/rules", requireAuth, adminOnly, firewallHandler.CreateRule)
		v1.DELETE("/firewall/rules/:ruleID", requireAuth, adminOnly, firewallHandler.DeleteRule)
		v1.GET("/firewall/precheck", requireAuth, adminOnly, firewallHandler.Precheck)
		v1.POST("/firewall/apply", requireAuth, adminOnly, firewallHandler.Apply)
		// 紧急回滚不设任何门槛。见 handler 上的说明。
		v1.POST("/firewall/rollback", requireAuth, adminOnly, firewallHandler.Rollback)

		v1.GET("/vms/:id/firewall", requireAuth, adminOnly, firewallHandler.GetVMPolicy)
		v1.PUT("/vms/:id/firewall", requireAuth, adminOnly, firewallHandler.SetVMPolicy)
		v1.DELETE("/vms/:id/firewall", requireAuth, adminOnly, firewallHandler.ClearVMPolicy)

		// 公网访问与开发模式开关（F-10-06）。
		//
		// 它**看起来像普通设置项但不是**：关掉公网访问时若调用方自己就在
		// 公网上，那一刻他的连接就断了——因此需要显式确认。而"是不是公网"
		// 靠请求方地址判断，判错的方向是相反的。
		//
		// 环境变量优先于面板（与 F-9-01 一致）：部署方在启动参数里关掉之后，
		// 面板上那个开关不该能把它打开。
		v1.GET("/settings/access", requireAuth, adminOnly, accessControlHandler.Get)
		v1.PUT("/settings/access", requireAuth, adminOnly, accessControlHandler.Set)

		// 平台自检与修复（F-4-13）。
		//
		// 自检与探测的区别：探测回答「有没有装」，自检回答「我们配的东西
		// 现在还在不在」——**面板显示「已启用」而节点上早就没了**，那种
		// 状态不会以任何形式报警，自检正是去找它。
		v1.GET("/network/client-ip", requireAuth, adminOnly, platformCheckHandler.ClientIP)
		v1.GET("/ovs/status", requireAuth, adminOnly, platformCheckHandler.OVSStatus)
		v1.GET("/ovs/ports", requireAuth, adminOnly, platformCheckHandler.OVSPorts)
		v1.GET("/ovs/leases", requireAuth, adminOnly, platformCheckHandler.Leases)
		v1.POST("/ovs/check", requireAuth, adminOnly, platformCheckHandler.Check)
		v1.POST("/ovs/repair", requireAuth, adminOnly, platformCheckHandler.Repair)

		// 宿主机性能调优（KSM / ZRAM / 嵌套虚拟化 / CPU 亲和）。
		//
		// 归管理员：这几项改的是**宿主机自己**的行为——ZRAM 会占掉一块
		// 物理内存、嵌套虚拟化会向所有来宾暴露虚拟化扩展。租户不该有能力
		// 影响同宿主上别人的机器。
		v1.GET("/host/tuning", requireAuth, adminOnly, tuningHandler.Get)
		v1.PUT("/host/tuning", requireAuth, adminOnly, tuningHandler.Apply)
		v1.GET("/host/cpu-affinity-presets", requireAuth, adminOnly, tuningHandler.ListPresets)
		v1.POST("/host/cpu-affinity-presets", requireAuth, adminOnly, tuningHandler.CreatePreset)
		v1.DELETE("/host/cpu-affinity-presets/:id", requireAuth, adminOnly, tuningHandler.DeletePreset)

		// PCIe 直通（GPU 等）。
		//
		// 归管理员：绑定设备会**改变宿主机上设备的归属**——把一块卡从宿主
		// 驱动抢过来，宿主就看不到它了。若那块卡正被宿主使用（例如唯一的
		// 网卡），绑定会让宿主机**当场失去网络**，而面板正是通过网络管理的。
		//
		// 挂载到虚拟机**要求关机**：直通设备不支持热插拔，而失败方式是
		// 一台机器卡在半启动状态。
		v1.GET("/host/passthrough", requireAuth, adminOnly, passthroughHandler.Overview)
		v1.POST("/host/passthrough/bind", requireAuth, adminOnly, passthroughHandler.Bind)
		v1.POST("/host/passthrough/unbind", requireAuth, adminOnly, passthroughHandler.Unbind)
		v1.GET("/vms/:id/passthrough", requireAuth, adminOnly, passthroughHandler.ListVM)
		v1.POST("/vms/:id/passthrough", requireAuth, adminOnly, passthroughHandler.Attach)
		v1.DELETE("/vms/:id/passthrough", requireAuth, adminOnly, passthroughHandler.Detach)

		// 宿主机防火墙（F-4-11 第一层）。
		//
		// **与 /firewall/* 是两套**：这里保护的是**宿主机自己与面板**（SSH、
		// 面板端口），而 /firewall/* 管的是虚拟机的入站流量。它们的配置项
		// 长得几乎一样，而攻击面完全不同——混在一处会让用户以为改了 KVM
		// 规则就关掉了面板的暴露面。
		//
		// 回滚**不需要二次验证**：它要在"已经出事了"的那一刻还能用。应用
		// 之后连不上面板或 SSH 断了，而用户又进不去宿主机时，这是唯一的
		// 自救入口。
		v1.GET("/host-firewall", requireAuth, adminOnly, hostFirewallHandler.Get)
		v1.PATCH("/host-firewall/policy", requireAuth, adminOnly, hostFirewallHandler.UpdatePolicy)
		v1.POST("/host-firewall/rules", requireAuth, adminOnly, hostFirewallHandler.CreateRule)
		v1.DELETE("/host-firewall/rules/:id", requireAuth, adminOnly, hostFirewallHandler.DeleteRule)
		v1.GET("/host-firewall/precheck", requireAuth, adminOnly, hostFirewallHandler.Precheck)
		v1.POST("/host-firewall/apply", requireAuth, adminOnly, hostFirewallHandler.Apply)
		v1.POST("/host-firewall/rollback", requireAuth, adminOnly, hostFirewallHandler.Rollback)
		v1.GET("/host-firewall/connections", requireAuth, adminOnly, hostFirewallHandler.Connections)
		v1.POST("/host-firewall/connections/close", requireAuth, adminOnly, hostFirewallHandler.CloseConnection)

		// 资源配额与超限处置（F-4-10）。
		//
		// 归管理员：配额是**跨用户的资源分配**，一个租户给自己调额度等于
		// 没有配额。
		//
		// 「提高上限会清除处置状态」写进了接口说明：不清的话，用户明明已经
		// 合规，网络却还是慢的，而界面上显示「已超限」。
		v1.GET("/resource-quotas", requireAuth, adminOnly, quotaEnforceHandler.List)
		v1.PUT("/resource-quotas", requireAuth, adminOnly, quotaEnforceHandler.Set)
		v1.DELETE("/resource-quotas/:id", requireAuth, adminOnly, quotaEnforceHandler.Delete)
		// 重置用量（F-1-09）：清的是**本周期的累计**，而不只是把状态改回正常。
		// 只清状态的话，下一次评估（五分钟内）会立刻重新判定为超限。
		v1.POST("/resource-quotas/:id/reset-usage", requireAuth, adminOnly, quotaEnforceHandler.ResetUsage)

		// 日志管理（F-9-02）。
		//
		// 归管理员：日志里有**全部请求的上下文**（谁在什么时候访问了什么）
		// 以及服务端内部的报错细节，对租户既无意义也不该被看到。
		//
		// 调级别与清理都记审计：把级别调到 DEBUG 会让日志量显著上升，
		// 而清理之后就无法回溯了——两者事后都要能回答「是谁在什么时候做的」。
		v1.GET("/settings/log/status", requireAuth, adminOnly, loggingHandler.Status)
		v1.GET("/settings/log/read", requireAuth, adminOnly, loggingHandler.Read)
		v1.PUT("/settings/log/level", requireAuth, adminOnly, loggingHandler.SetLevel)
		v1.GET("/settings/log/export", requireAuth, adminOnly, loggingHandler.Export)
		// 实时日志流（SSE）。见 GAP_CLOSURE 的 G-28 论证：日志在线查看是
		// 唯一「持续在变、而且停不下来」的那条流，也是唯一值得换成推送的地方。
		//
		// 鉴权仍是 Cookie——EventSource 对同源请求会自动带上它，因此不需要
		// 把令牌放进查询串。
		v1.GET("/settings/log/stream", requireAuth, adminOnly, loggingHandler.Stream)
		v1.POST("/settings/log/delete", requireAuth, adminOnly, loggingHandler.Delete)

		// 版本与关于（F-9-05）。
		//
		// **不限制角色**：内容是版本号与依赖清单，不含租户数据也不含密钥。
		// 它最常见的用途是「遇到问题先看一眼自己跑的是哪个版本」，把入口
		// 藏起来只会让用户去别处猜。
		v1.GET("/version", requireAuth, versionHandler.Get)

		// 接口清单（F-9-07）。
		//
		// 它**不标注角色要求**：那个信息无法从路由表里可靠地取出来，而猜
		// 出来的版本比没有更糟（写错一条就有人照着它去设计调用）。清单只
		// 保证「有哪些接口、怎么调」，鉴权要求以 docs/03-api/API.md 为准。
		v1.GET("/api-endpoints", requireAuth, apiDocsHandler.List)

		// 告警中心（F-8-07）：与工作台的横幅同源，但这里要能翻、能确认。
		v1.GET("/alerts", requireAuth, alertHandler.List)
		v1.POST("/alerts/:id/ack", requireAuth, alertHandler.Ack)
		v1.POST("/alerts/ack-all", requireAuth, alertHandler.AckAll)

		// 回收站（F-2-16）：删除只是把机器移出列表，彻底删除是这里的第二步。
		//
		// 三步里的前两步都**可逆**：移入回收站不动磁盘，恢复只是把记录放回
		// 列表。不可逆的（删盘 + 物理删记录）只发生在用户明确点「彻底删除」
		// 之后。
		v1.GET("/vms/trash", requireAuth, vmHandler.Trash)
		v1.POST("/vms/trash/:id/restore", requireAuth, vmHandler.Restore)
		v1.DELETE("/vms/trash/:id", requireAuth, vmHandler.Purge)

		// 跨资源检索（F-9-08）：按名字找虚拟机 / 节点 / 模板。
		//
		// **不限制角色**，但服务层按视角收敛结果——搜索框看着只是"帮你找
		// 东西"，实际能枚举出整个平台的资源名，因此它是最容易意外泄漏信息
		// 的入口之一。
		v1.GET("/search", requireAuth, searchHandler.Query)

		// 计算资源配额（vCPU / 内存 / 实例数）。
		//
		// 它与 resource-quotas 是**两类约束**：后者按周期累计、超限后限速或
		// 断网；这里看的是"此刻占着多少"，超限的处置是**拒绝新建**——把一台
		// 正在跑的机器限速掉，比不让它再建一台严重得多。
		v1.GET("/compute-quotas", requireAuth, adminOnly, computeQuotaHandler.List)
		v1.PUT("/compute-quotas", requireAuth, adminOnly, computeQuotaHandler.Set)

		// 诊断导出（F-9-03）。
		//
		// 归管理员：包里是整个系统的内部状态（全部设置、全部审计日志），
		// 而审计日志里含别人的操作记录。
		//
		// **不需要二次验证**，这是刻意的：它最常见的用法是"出问题的时候
		// 赶紧导出来发给支持"，而在那一刻再拦一道验证会让最需要它的时候
		// 最不好用。敢这样的前提是**脱敏发生在打包时**——包里不含密钥
		// 明文，本身就是可以外发的。
		v1.GET("/diagnostics/categories", requireAuth, adminOnly, diagnosticsHandler.Categories)
		v1.GET("/diagnostics/export", requireAuth, adminOnly, diagnosticsHandler.Export)

		// 抓包与网络诊断（F-4-12）。
		//
		// 抓包会读到一个网口上的**全部流量**（包括明文密码与会话令牌），
		// 因此：租户只能看与删自己发起的（列表在服务层按 created_by 过滤），
		// 而每一次抓包都进审计。
		v1.GET("/captures", requireAuth, captureHandler.List)
		v1.POST("/captures", requireAuth, captureHandler.Start)
		v1.DELETE("/captures/:id", requireAuth, captureHandler.Delete)

		// 端口安全（F-4-08）。
		//
		// 归管理员：它会改虚拟机所在**二层网段**的连通性——一个租户给自己
		// 开端口隔离，影响的是同网段所有机器（包括别人的），而"同网段"
		// 这件事在界面上看不出来，租户无法判断自己会波及谁。
		//
		// 启用走**预检 → 确认**：同网段失联是不可从界面直接看出来的后果，
		// 而能力是否具备更不能等到点下"启用"才告诉用户。
		v1.GET("/port-security", requireAuth, adminOnly, portSecurityHandler.List)
		v1.POST("/port-security/preview", requireAuth, adminOnly, portSecurityHandler.Preview)
		v1.POST("/port-security", requireAuth, adminOnly, portSecurityHandler.Apply)
		v1.DELETE("/port-security/:id", requireAuth, adminOnly, portSecurityHandler.Disable)

		// 调度器框架（F-7-04）。
		//
		// 归管理员：这里暴露的是**系统内部的运行细节**（有哪些周期任务、
		// 最近做了什么、哪些失败了）。它对运维排查有用，对租户没有意义。
		//
		// 两个接口都只读——调度器是代码里注册的，不能从界面上开关。
		// 一个能被随手关掉的指标采集器，会让「指标为什么断了」变成一个
		// 需要翻操作日志才能回答的问题。
		v1.GET("/schedulers", requireAuth, adminOnly, schedulerHandler.Overview)
		v1.GET("/scheduler-events", requireAuth, adminOnly, schedulerHandler.Events)

		// 存储卷（F-5-02，LVM 多盘聚合）。
		//
		// 归管理员：卷会**独占物理设备**，选错盘会影响这台宿主机上所有
		// 虚拟机的存储。与存储池同一档。
		//
		// 创建走**预检 → 确认**两步：条带与镜像的组合里有一个直觉容易
		// 出错的乘法（需要 stripe × mirror 块盘），而"条带没有冗余"更是
		// 几乎所有人都会有的误解——看到「用了 4 块盘」很自然会以为那是
		// 4 块盘的冗余。
		v1.GET("/storage-volumes", requireAuth, adminOnly, volumeHandler.List)
		v1.POST("/storage-volumes/preview", requireAuth, adminOnly, volumeHandler.Preview)
		v1.POST("/storage-volumes", requireAuth, adminOnly, volumeHandler.Create)
		// 删除**会销毁卷里的全部数据**，不可恢复。
		v1.DELETE("/storage-volumes/:id", requireAuth, adminOnly, volumeHandler.Delete)

		// 用户存储空间与文件管理（F-5-03/04/05）。
		//
		// 上传分两步：创建会话（可命中**秒传**）→ 登记分片 → 收尾。
		// 会话把「已经收到了哪些分片」变成可持久化的事实，于是断点续传
		// 只是「问服务端我还要传哪几片」。
		v1.GET("/my-storage", requireAuth, userStorageHandler.Get)
		v1.POST("/my-storage", requireAuth, userStorageHandler.Ensure)
		v1.GET("/my-storage/files", requireAuth, userStorageHandler.ListFiles)
		v1.DELETE("/my-storage/files/:id", requireAuth, userStorageHandler.DeleteFile)
		v1.POST("/my-storage/uploads", requireAuth, userStorageHandler.CreateUpload)
		v1.GET("/my-storage/uploads/:uploadID", requireAuth, userStorageHandler.GetUpload)
		v1.POST("/my-storage/uploads/:uploadID/complete", requireAuth, userStorageHandler.CompleteUpload)
		// 分片**字节**：请求体是裸二进制，不走 JSON。
		v1.PUT("/my-storage/uploads/:uploadID/chunks/:index", requireAuth, userStorageHandler.PutChunkData)

		// 目录共享到虚拟机（F-5-06，9p VirtFS）。
		//
		// **接口只接受相对路径**（相对于调用者的存储根），这是整块功能的
		// 安全前提：共享是一个跨越虚拟化边界的读取入口，而参数来自用户。
		// 允许绝对路径意味着一个租户可以把 /etc 挂进自己的虚拟机读出来，
		// 而这不会触发任何权限检查——qemu 是以一个有权读它的用户在跑。
		//
		// 卸载不需要二次验证：目录还在宿主机上，随时可以再挂回去。
		v1.GET("/vms/:id/shares", requireAuth, storageHandler.ListShares)
		v1.POST("/vms/:id/shares", requireAuth, storageHandler.MountShare)
		v1.DELETE("/vms/:id/shares/:tag", requireAuth, storageHandler.UnmountShare)

		// 安全组（F-4-03）与 ACL 汇总应用（F-4-04）。
		//
		// **组内只有允许规则**：叠加生效意味着生效规则是各组的并集，而在
		// 并集模型里「拒绝」没法定义——A 组拒绝 22、B 组允许 22，合并后通不通
		// 取决于谁先算，而那是用户看不见的实现细节。默认拒绝由组的整体语义
		// 给出：不在任何允许规则里的流量一律不通。
		v1.GET("/security-groups", requireAuth, sgHandler.ListGroups)
		v1.POST("/security-groups", requireAuth, sgHandler.CreateGroup)
		v1.DELETE("/security-groups/:id", requireAuth, sgHandler.DeleteGroup)
		v1.GET("/security-groups/:id/rules", requireAuth, sgHandler.ListRules)
		v1.POST("/security-groups/:id/rules", requireAuth, sgHandler.CreateRule)
		v1.PATCH("/security-groups/:id/rules/:ruleID", requireAuth, sgHandler.UpdateRule)
		v1.DELETE("/security-groups/:id/rules/:ruleID", requireAuth, sgHandler.DeleteRule)
		v1.POST("/security-groups/:id/interfaces/:interfaceID", requireAuth, sgHandler.Attach)
		v1.DELETE("/security-groups/:id/interfaces/:interfaceID", requireAuth, sgHandler.Detach)

		// 生效规则的汇总与下发（F-4-04）。应用必须带回预览版本号：
		// 用户确认的必须正是他看到的那一套规则。
		v1.GET("/vms/:id/security-groups/effective", requireAuth, sgHandler.Effective)
		v1.POST("/vms/:id/security-groups/apply", requireAuth, sgHandler.Apply)

		// 公网 IP（F-4-06）。
		//
		// 地址池是**节点级资源**，整体归 admin（与节点、存储池同一档）。
		// 「谁能绑哪个地址」属于配额范畴（f-1-08 把公网 IP 列为配额维度），
		// 那是后续的事——先让地址能录进来、能绑上去。
		//
		// 绑定、解绑、迁移都不需要二次验证：它们都不会造成不可逆的结果，
		// 而连通性中断是**立刻可见**的——用户马上就知道发生了什么。
		v1.GET("/public-ips", requireAuth, adminOnly, publicIPHandler.List)
		v1.POST("/public-ips", requireAuth, adminOnly, publicIPHandler.Create)
		// 批量操作。**必须注册在带 :id 的路由之前**，否则 `batch` 会被
		// 当成一个 id 去匹配 `/public-ips/:id/bind`。
		//
		// 它们逐条调用单条方法，复用全部校验——在批量里重写一遍的话两处
		// 迟早分叉，而分叉的表现是「单个能解绑、批量里解不了」。
		v1.POST("/public-ips/batch/bind", requireAuth, adminOnly, publicIPHandler.BatchBind)
		v1.POST("/public-ips/batch/unbind", requireAuth, adminOnly, publicIPHandler.BatchUnbind)
		v1.DELETE("/public-ips/:id", requireAuth, adminOnly, publicIPHandler.Delete)
		v1.GET("/public-ips/:id/preview", requireAuth, adminOnly, publicIPHandler.Preview)
		v1.POST("/public-ips/:id/bind", requireAuth, adminOnly, publicIPHandler.Bind)
		v1.POST("/public-ips/:id/migrate", requireAuth, adminOnly, publicIPHandler.Migrate)
		v1.DELETE("/public-ips/:id/bind", requireAuth, adminOnly, publicIPHandler.Unbind)

		// 存储配额（F-9-02）。
		//
		// 自己的用量对所有登录用户开放；设置配额与查看全部用户的用量是
		// admin 专属——用量数字会暴露「谁有多少资源」。
		v1.GET("/quota", requireAuth, quotaHandler.Mine)
		v1.GET("/quotas", requireAuth, adminOnly, quotaHandler.List)
		v1.PUT("/quotas", requireAuth, adminOnly, quotaHandler.Set)

		// 来宾自动化（F-2-10）。四种动作共用一个入口。
		//
		// 不需要二次验证：改密与附加磁盘都**不是不可逆的数据操作**——
		// 密码可以再改回来，附加磁盘影响的是新盘（既有数据不受影响），
		// 扩容只是把盘变大。给可逆操作加验证只会稀释验证本身的分量。
		v1.GET("/vms/:id/guest-actions", requireAuth, vmHandler.GuestCapabilities)
		v1.POST("/vms/:id/guest-actions", requireAuth, vmHandler.GuestAction)

		// 导出（F-2-14）与产物下载。
		//
		// 都不需要二次验证：导出是只读地把系统盘打成镜像，删产物删的是一份
		// 副本——两者都不影响虚拟机本身。
		v1.GET("/vms/:id/exports", requireAuth, vmHandler.Exports)
		v1.POST("/vms/:id/exports", requireAuth, vmHandler.CreateExport)
		v1.DELETE("/vms/:id/exports/:exportID", requireAuth, vmHandler.DeleteExport)
		v1.GET("/vms/:id/exports/:exportID/download", requireAuth, vmHandler.DownloadExport)

		// 重装系统（F-2-11）。重建走二次验证（整块系统盘被替换，不可逆）；
		// 清理备份不需要——它删的是已不再被使用的备份，当前运行不受影响。
		v1.POST("/vms/:id/reinstall", requireAuth, vmHandler.Reinstall)
		v1.DELETE("/vms/:id/reinstall/backup", requireAuth, vmHandler.PurgeReinstallBackup)

		// 救援系统（F-2-12）。进入与退出都是任务：两者都要改虚拟机的硬件
		// 配置并重启，是宿主机上的实际操作。
		v1.POST("/vms/:id/rescue", requireAuth, vmHandler.EnterRescue)
		v1.DELETE("/vms/:id/rescue", requireAuth, vmHandler.ExitRescue)

		// Hero 的资源卡与控制台预览卡（f-2-01 §5.3.3）。
		//
		// 两者都是**只读探测**，不入队、不写投影：指标与画面都是瞬时的，
		// 存下来只会在下一次读取时给出一个过期的答案。
		v1.GET("/vms/:id/stats", requireAuth, vmHandler.Stats)
		// 详情页的三块补充能力：事件时间线、PCIe 槽位余量、邻居表。三者都是**按需**取——它们各需要一次向节点的请求或一次流水查询，塞进详情会让每次打开详情页都变慢。
		v1.GET("/vms/:id/timeline", requireAuth, vmHandler.Timeline)
		v1.GET("/vms/:id/pcie-info", requireAuth, vmHandler.PCIeInfo)
		v1.GET("/vms/:id/neighbors", requireAuth, vmHandler.Neighbors)
		v1.GET("/vms/:id/console/frame", requireAuth, vmHandler.ConsoleFrame)
		// 电源与删除都是异步操作：受理时校验状态并返回任务标识，执行由
		// 任务队列按资源锁串行（f-2-01 R-005）。
		v1.POST("/vms/:id/power-actions", requireAuth, vmHandler.Power)
		v1.DELETE("/vms/:id", requireAuth, vmHandler.Delete)
		// 分配归属（F-1-07）：把已有虚拟机指派给某个用户。仅管理员。
		v1.PUT("/vms/:id/owner", requireAuth, vmHandler.AssignOwner)

		// 详情页「网络管理」标签页（F-2-03）。读接口直接返回投影；
		// 写接口全部入队——它们都要下发到节点，且资源锁与电源操作共用
		// vm:<id>，因此不会出现「改完网卡正好赶上关机」。
		v1.GET("/vms/:id/interfaces", requireAuth, vmHandler.Interfaces)
		v1.GET("/vms/:id/static-ips", requireAuth, vmHandler.StaticIPs)
		v1.POST("/vms/:id/interfaces", requireAuth, vmHandler.AddInterface)
		v1.PATCH("/vms/:id/interfaces/:nicID", requireAuth, vmHandler.UpdateInterface)
		v1.DELETE("/vms/:id/interfaces/:nicID", requireAuth, vmHandler.RemoveInterface)
		v1.POST("/vms/:id/static-ips", requireAuth, vmHandler.BindStaticIP)
		v1.DELETE("/vms/:id/static-ips/:ipID", requireAuth, vmHandler.UnbindStaticIP)
		// 批量删除端口转发。逐条调用单条方法，复用全部校验（尤其是归属）。
		// 注册在 `:id` 路由之前，理由同上。
		v1.POST("/vms/port-forwards/batch-delete", requireAuth, vmHandler.BatchRemovePortForwards)
		v1.GET("/vms/:id/port-forwards", requireAuth, vmHandler.PortForwards)
		v1.POST("/vms/:id/port-forwards", requireAuth, vmHandler.AddPortForward)
		v1.DELETE("/vms/:id/port-forwards/:pfID", requireAuth, vmHandler.RemovePortForward)

		// 编辑配置（F-2-05）。元数据与硬件分开：前者是纯控制面数据，
		// 同步改库即可；后者要下发到节点，走任务队列。
		v1.GET("/vms/:id/edit-form", requireAuth, vmHandler.EditForm)
		v1.PATCH("/vms/:id/metadata", requireAuth, vmHandler.UpdateMetadata)
		v1.POST("/vms/:id/config-changes", requireAuth, vmHandler.UpdateConfig)

		// 快照（F-2-07）。三个动作全部走任务队列：创建与恢复要复制或回滚
		// 整个磁盘镜像，同步等待必然超时（f-7-01 R-001）。
		v1.GET("/vms/:id/snapshots", requireAuth, vmHandler.Snapshots)
		v1.POST("/vms/:id/snapshots", requireAuth, vmHandler.CreateSnapshot)
		v1.POST("/vms/:id/snapshots/:snapshotID/restore", requireAuth, vmHandler.RestoreSnapshot)
		v1.DELETE("/vms/:id/snapshots/:snapshotID", requireAuth, vmHandler.DeleteSnapshot)
		// 删除全部快照（F-2-07）。受**二次验证**保护：丢一个还原点与丢掉
		// 整条时间线不是同一件事，而点这个按钮的人多半没有逐条确认过。
		v1.POST("/vms/:id/snapshots/delete-all", requireAuth, vmHandler.DeleteAllSnapshots)
		// UEFI 启动项修复（F-2-11）。恢复快照之后的典型后果就是它。
		v1.POST("/vms/:id/nvram/repair", requireAuth, vmHandler.RepairNVRAM)

		// 定时任务（F-7-05）。执行时复用已有的 vm.power / vm.delete 任务，
		// 因此不需要新的执行器。删除类任务在**创建时**走二次验证——它是
		// 一条将来会自动执行的删除指令，留到执行时再验证就没人可验了。
		v1.GET("/vms/:id/schedules", requireAuth, scheduleHandler.List)
		v1.POST("/vms/:id/schedules", requireAuth, scheduleHandler.Create)
		v1.PATCH("/vms/:id/schedules/:scheduleID", requireAuth, scheduleHandler.SetEnabled)
		v1.DELETE("/vms/:id/schedules/:scheduleID", requireAuth, scheduleHandler.Delete)

		// 控制台（F-2-08）。WebSocket 端点同样经过认证中间件：Cookie 随
		// 握手请求发送，因此**升级前**就完成了鉴权与授权（R-003）——
		// 升级之后没有 HTTP 状态码可用，那时再拒绝已经没有合适的表达方式。
		//
		// 「对外暴露」是受二次验证保护的高危操作，由 handler 按请求内容
		// 决定是否要求验证（只改暴露状态时才要求）。
		v1.GET("/vms/:id/console", requireAuth, consoleHandler.GetConfig)
		v1.PATCH("/vms/:id/console", requireAuth, consoleHandler.Update)
		v1.GET("/vms/:id/console/screenshot", requireAuth, consoleHandler.Screenshot)
		v1.GET("/vms/:id/console/ws", requireAuth, consoleHandler.WS)
		// SPICE 连接文件（.vv）。含明文密码的版本需要二次验证，由 handler
		// 按查询参数决定——默认不含密码，用户手输即可。
		v1.GET("/vms/:id/console/connection-file", requireAuth, consoleHandler.ConnectionFile)
		// 虚拟机定义（**只读**）。排查「面板显示的和实际跑的不是一回事」时
		// 它是唯一的真相，因此总是现读、不缓存。返回内容已脱敏——libvirt
		// 的定义里有控制台密码，原样送出等于把界面上「只写不读」的凭据
		// 从后门送出去。
		v1.GET("/vms/:id/xml", requireAuth, consoleHandler.XML)
		// 关机状态下的磁盘扩容（**只能扩，不能缩**）。运行中的扩容走
		// 「来宾自动化」里的 expand_disk——那条路会顺带在来宾里扩好文件系统。
		v1.POST("/vms/:id/disk/resize", requireAuth, vmHandler.ResizeDisk)
		// 磁盘管理（F-2-06）：列表为实时探测，变更为入队操作。
		//
		// 三件事刻意分开成三种资源语义：扩容只能扩（缩容必坏）、卸载不能
		// 碰系统盘、换总线必须关机。它们各自的约束差别很大，合并成一个
		// 「改磁盘」接口会把这些约束挤成一句笼统的"不允许"。
		v1.GET("/vms/:id/disks", requireAuth, vmHandler.Disks)
		v1.POST("/vms/:id/disks", requireAuth, vmHandler.ChangeDisk)
		// 把链接克隆的磁盘变为独立盘（**需要停机**）。它存在的理由是
		// 链接克隆的父盘删不掉，而模板的管理需要能删掉旧的父盘。
		v1.POST("/vms/:id/disks/independent", requireAuth, vmHandler.MakeDisksIndependent)
		// 批量克隆（一次最多 5 台）。限制与存储 IO 有关，理由写在报错里。
		v1.POST("/vms/batch-clone", requireAuth, vmHandler.BatchClone)

		// 迁移预检（**只读**）。它回答用户点下按钮之前唯一想知道的那件事：
		// 这次要停多久。因此除了「能不能迁」，还给出「会怎么迁」——
		// 停机时长由后者决定。**与迁移共用同一套校验**。
		v1.POST("/vms/:id/migration/preview", requireAuth, vmHandler.PreviewMigration)

		// 光驱（可以有多个）。**弹出与移除是两件事**：弹出之后光驱仍在
		// （来宾里看得到一个空的托盘），移除才是设备消失。
		// **换盘之后来宾通常看不到新介质**（多数系统缓存了介质信息），
		// 界面要提示可以重新挂载或重启；**换总线通常需要重启**。
		v1.GET("/vms/:id/cdroms", requireAuth, vmHandler.CDROMs)
		v1.POST("/vms/:id/cdroms", requireAuth, vmHandler.AttachCDROM)
		v1.PUT("/vms/:id/cdroms/:cdromID/iso", requireAuth, vmHandler.LoadCDROM)
		v1.POST("/vms/:id/cdroms/:cdromID/eject", requireAuth, vmHandler.EjectCDROM)
		v1.PUT("/vms/:id/cdroms/:cdromID/bus", requireAuth, vmHandler.SetCDROMBus)
		v1.DELETE("/vms/:id/cdroms/:cdromID", requireAuth, vmHandler.RemoveCDROM)
		// 校验并给出 diff（**只读**，不需要二次验证——不产生改动，而
		// "看一眼会影响什么"如果需要先验证一次，用户就会在还不知道要改
		// 什么的时候被迫走一遍验证流程）。
		v1.POST("/vms/:id/xml/precheck", requireAuth, consoleHandler.XMLPrecheck)
		// 应用。**需要二次验证**：它绕过我们建立的其它全部校验。
		v1.PUT("/vms/:id/xml", requireAuth, consoleHandler.XMLUpdate)

		// 任务中心：tenant 只能看到自己发起的（同样由归属过滤保证）。
		v1.GET("/tasks", requireAuth, taskHandler.List)
		// 任务状态流（SSE）。它替代任务中心与底部任务栏的轮询。
		v1.GET("/tasks/stream", requireAuth, taskHandler.Stream)
		v1.GET("/tasks/:id", requireAuth, taskHandler.Get)
		v1.POST("/tasks/:id/cancel", requireAuth, taskHandler.Cancel)
		// 清理已完成的旧任务。**只清终态**，且**被引用的不删**——
		// vm_schedule.last_task_id 与 network_capture.task_id 是当前状态
		// 的一部分，清掉它们指向的任务之后那些引用会悬空。
		v1.POST("/tasks/clear", requireAuth, adminOnly, taskHandler.ClearTasks)

		if deps.SimulateAgent {
			// 开发期专用：调用它与节点走**同一段注册逻辑**（ADR-0007）。
			// 它不需要认证——节点注册也不凭用户身份，而凭注册令牌。
			v1.POST("/dev/agent-register", nodeHandler.SimulateRegister)
		}
	}
}
