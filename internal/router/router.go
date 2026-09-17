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

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/handler"
	"k_cockpit/internal/network"
	"k_cockpit/internal/node"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/schedule"
	"k_cockpit/internal/settings"
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
	"k_cockpit/internal/template"
	"k_cockpit/internal/vm"
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
	// Risk 强制高风险操作的二次验证（f-10-01）。受保护的操作在 handler
	// 入口调用它，清单本身集中在 internal/risk。
	Risk *risk.Guard
	// Schedule 提供虚拟机的定时任务（F-7-05）。
	Schedule *schedule.Service
	// Template 提供模板管理与模板克隆（F-3-01 / F-3-02）。
	Template *template.Service

	SecureCookie bool
	// SimulateAgent 为 true 时注册开发期的模拟注册入口。
	// 仅在 AGENT_TRANSPORT=mock 时开启；接入真实 agent 后应关闭。
	SimulateAgent bool
}

// Register 注册全局中间件与全部路由。
func Register(h *server.Hertz, deps Deps) {
	// 顺序：RequestID 最先（后续都要用它）；Recover 包裹业务处理，
	// 保证 panic 也被记录 request_id；AccessLog 在最内层以准确统计耗时。
	h.Use(api.RequestID(), api.Recover(), api.AccessLog())

	h.GET("/health", handler.Health(deps.DB))

	authHandler := handler.NewAuth(deps.Auth, deps.SecureCookie, deps.Risk)
	setupHandler := handler.NewSetup(deps.Bootstrap, deps.Auth, deps.SecureCookie)
	nodeHandler := handler.NewNode(deps.Node, deps.SimulateAgent, deps.Risk)
	vmHandler := handler.NewVM(deps.VM, deps.Risk)
	taskHandler := handler.NewTask(deps.Task)
	securityHandler := handler.NewSecurity(deps.Risk, deps.Auth)
	scheduleHandler := handler.NewSchedule(deps.Schedule, deps.Risk)
	storageHandler := handler.NewStorage(deps.Storage, deps.Risk)
	networkHandler := handler.NewNetwork(deps.Network)
	settingsHandler := handler.NewSettings(deps.Settings)
	consoleHandler := handler.NewConsole(deps.VM, deps.Risk)
	templateHandler := handler.NewTemplate(deps.Template)

	authMW := auth.NewMiddleware(deps.Auth)
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

		v1.POST("/auth/logout", requireAuth, authHandler.Logout)
		v1.GET("/auth/session", requireAuth, authHandler.Session)
		v1.GET("/auth/sessions", requireAuth, authHandler.Sessions)
		v1.DELETE("/auth/sessions/:id", requireAuth, authHandler.RevokeSession)

		// 高风险二次验证（f-10-01）：清单只读，验证接口换取一次性许可。
		// 清单是**唯一事实来源**，前端不得硬编码第二份（R-002）。
		v1.GET("/security/high-risk-policy", requireAuth, securityHandler.Policy)
		v1.POST("/auth/risk-verification", requireAuth, securityHandler.Verify)

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

		// 网络（F-4-01）：M2 只有只读接口——能力探测与降级说明。
		// 网络变更（建网桥、物理口入桥）属 M3 范围。
		v1.GET("/nodes/:id/network", requireAuth, adminOnly, networkHandler.Status)
		v1.GET("/nodes/:id/networks", requireAuth, adminOnly, networkHandler.Networks)

		// 虚拟交换机（F-4-02）。写操作走任务队列——建网桥是宿主机上的实际
		// 操作，接口不同步等待；记录由执行器在节点成功后写入。
		//
		// 不需要二次验证：交换机变更**可逆**（改回去即可），而它影响的是
		// 网络连通性而非数据。给可逆操作加验证只会稀释验证本身的分量。
		v1.POST("/nodes/:id/vpc-switches", requireAuth, adminOnly, networkHandler.CreateSwitch)
		v1.PATCH("/vpc-switches/:id", requireAuth, adminOnly, networkHandler.UpdateSwitch)
		v1.DELETE("/vpc-switches/:id", requireAuth, adminOnly, networkHandler.DeleteSwitch)

		// 系统设置（F-9-01）：**仅管理员**（R-014）。设置变更不得成为
		// 绕过权限的通道，因此 tenant 连可见性都没有。
		v1.GET("/settings", requireAuth, adminOnly, settingsHandler.List)
		v1.PATCH("/settings", requireAuth, adminOnly, settingsHandler.Update)
		v1.POST("/settings/rollback", requireAuth, adminOnly, settingsHandler.Rollback)

		// 虚拟机：管理员可操作全部，tenant 仅自己名下（归属过滤在数据访问层注入，
		// 因此这里不需要按角色分路由）。
		v1.GET("/vms", requireAuth, vmHandler.List)
		v1.GET("/vms/:id", requireAuth, vmHandler.Get)
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
		v1.DELETE("/templates/:id", requireAuth, templateHandler.Delete)

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
		v1.GET("/vms/:id/console/frame", requireAuth, vmHandler.ConsoleFrame)
		// 电源与删除都是异步操作：受理时校验状态并返回任务标识，执行由
		// 任务队列按资源锁串行（f-2-01 R-005）。
		v1.POST("/vms/:id/power-actions", requireAuth, vmHandler.Power)
		v1.DELETE("/vms/:id", requireAuth, vmHandler.Delete)

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

		// 任务中心：tenant 只能看到自己发起的（同样由归属过滤保证）。
		v1.GET("/tasks", requireAuth, taskHandler.List)
		v1.GET("/tasks/:id", requireAuth, taskHandler.Get)
		v1.POST("/tasks/:id/cancel", requireAuth, taskHandler.Cancel)

		if deps.SimulateAgent {
			// 开发期专用：调用它与真实 agent 走**同一段注册逻辑**（ADR-0007）。
			// 它不需要认证——真实 agent 注册也不凭用户身份，而凭注册令牌。
			v1.POST("/dev/agent-register", nodeHandler.SimulateRegister)
		}
	}
}
