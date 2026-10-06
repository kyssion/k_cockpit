// Package router 集中注册 HTTP 路由。
//
// 所有对外暴露的接口都在这里登记，便于一眼看清服务的 API 面，也便于
// 集中审查「哪些接口需要什么角色」（f-1-06 R-003：角色要求在这里声明，
// 而不是散落在各 handler 内部判断）。
//
// 路由声明按业务域拆在 routes_<域>.go 里（与 handler/、service/ 的域子包
// 同名对应）；本文件只剩全局中间件、认证守卫的装配与各域注册的调度。
// 业务接口随功能实现逐步接入，并在 docs/03-api/API.md 的接口清单中登记。
package router

import (
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"k_cockpit/internal/handler/platform"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/platform/logging"
	"k_cockpit/internal/service/compute/computequota"
	"k_cockpit/internal/service/compute/importer"
	"k_cockpit/internal/service/compute/passthrough"
	"k_cockpit/internal/service/compute/template"
	"k_cockpit/internal/service/compute/vm"
	"k_cockpit/internal/service/compute/vmtag"
	"k_cockpit/internal/service/network/bridge"
	"k_cockpit/internal/service/network/capture"
	"k_cockpit/internal/service/network/firewall"
	"k_cockpit/internal/service/network/hostfirewall"
	"k_cockpit/internal/service/network/portmirror"
	"k_cockpit/internal/service/network/portsecurity"
	"k_cockpit/internal/service/network/publicip"
	"k_cockpit/internal/service/network/securitygroup"
	"k_cockpit/internal/service/network/vpcacl"
	"k_cockpit/internal/service/network/vswitch"
	"k_cockpit/internal/service/node"
	"k_cockpit/internal/service/ops/alert"
	"k_cockpit/internal/service/ops/dashboard"
	"k_cockpit/internal/service/ops/diagnostics"
	"k_cockpit/internal/service/ops/hosttuning"
	"k_cockpit/internal/service/ops/maintenance"
	"k_cockpit/internal/service/ops/monitor"
	"k_cockpit/internal/service/ops/platformcheck"
	"k_cockpit/internal/service/ops/quotaenforce"
	"k_cockpit/internal/service/ops/realtime"
	"k_cockpit/internal/service/ops/schedule"
	"k_cockpit/internal/service/ops/scheduler"
	"k_cockpit/internal/service/ops/search"
	"k_cockpit/internal/service/ops/task"
	"k_cockpit/internal/service/platform/accesscontrol"
	"k_cockpit/internal/service/platform/apikey"
	"k_cockpit/internal/service/platform/auditlog"
	"k_cockpit/internal/service/platform/auth"
	"k_cockpit/internal/service/platform/authkey"
	"k_cockpit/internal/service/platform/invite"
	"k_cockpit/internal/service/platform/mailer"
	"k_cockpit/internal/service/platform/passaudit"
	"k_cockpit/internal/service/platform/reqlog"
	"k_cockpit/internal/service/platform/risk"
	"k_cockpit/internal/service/platform/settings"
	"k_cockpit/internal/service/platform/useradmin"
	"k_cockpit/internal/service/storage/pool"
	"k_cockpit/internal/service/storage/quota"
	"k_cockpit/internal/service/storage/userstorage"
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
	Storage   *pool.Service
	Network   *vswitch.Service
	Settings  *settings.Service
	Task      *task.Queue
	// Mailer 提供发信能力（F-1-08）。为 nil 时测试发信接口返回不可用。
	Mailer *mailer.Service
	// Bus 是实时事件总线（F-7-02 的任务流）。为 nil 时实时通道返回"未启用"。
	Bus *realtime.Bus
	// VpcACL 提供 VPC 网络的访问控制（F-4-05）。为 nil 时 ACL 接口不可用。
	VpcACL *vpcacl.Service
	// ReqLog 记录与查询接口调用日志（F-10-07）。为 nil 时不记录、接口不可用。
	ReqLog *reqlog.Service
	// PassAudit 提供弱口令 / 泄露口令检查（F-10-06）。为 nil 时接口不可用。
	PassAudit *passaudit.Service
	// AuthKey 提供会话签名密钥轮换（F-1-09）。为 nil 时接口不可用。
	AuthKey *authkey.Service
	// Invite 提供邀请注册（F-1-10）。为 nil 时接口不可用。
	Invite *invite.Service
	// Risk 强制高风险操作的二次验证（f-10-01）。受保护的操作在 handler
	// 入口调用它，清单本身集中在 service/platform/risk。
	Risk *risk.Guard
	// Schedule 提供虚拟机的定时任务（F-7-05）。
	Schedule *cron.Service
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
	NetworkBridge *bridge.Service
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
	// Maintenance 提供站点级维护模式（G-46）。
	Maintenance *maintenance.Service

	SecureCookie bool
	// SimulateAgent 为 true 时注册开发期的模拟注册入口。
	// 仅在 AGENT_TRANSPORT=mock 时开启；该开关为 mock 专用。
	SimulateAgent bool
	// InputFilterEnabled 返回输入侧防护（请求过滤）是否开启；nil 时恒开。
	// 做成函数而不是布尔：开关来自系统设置，需要每次请求现读。
	InputFilterEnabled func() bool
}

// guards 聚合路由级中间件，供各域注册函数复用。
type guards struct {
	requireAuth app.HandlerFunc
	adminOnly   app.HandlerFunc
}

// Register 注册全局中间件与全部路由。
func Register(h *server.Hertz, deps Deps) {
	// 顺序：RequestID 最先（后续都要用它）；Recover 包裹业务处理，
	// 保证 panic 也被记录 request_id；AccessLog 在最内层以准确统计耗时。
	//
	// RequestLogger 放在**最外层**：这样连 401 也能记到，而"为什么一直 401"
	// 恰恰是最需要看请求日志的场景。它内部按开关决定是否落库。
	if deps.ReqLog != nil {
		h.Use(platform.RequestLogger(deps.ReqLog))
	}
	h.Use(api.SecurityHeaders(), api.InputFilter(deps.InputFilterEnabled))
	h.Use(api.RequestID(), api.Recover(), api.AccessLog())

	h.GET("/health", platform.Health(deps.DB))

	// 登录阶段的二次验证复用 risk 的校验逻辑（恢复码一次性、TOTP 容差与
	// 算法参数只有一处定义）。接线放在这里而不是启动脚本里：漏接的表现是
	// 「登录时永远提示服务不可用」，而它不会在编译期暴露。
	if deps.Auth != nil && deps.Risk != nil {
		deps.Auth.SetLoginVerifier(deps.Risk)
	}

	// 挂上 API 凭证认证：客户端可用 `Authorization: Bearer kc_...` 代替会话 Cookie。
	//
	// 注销、改密码这类会话管理接口在凭证认证下会拿到 nil 会话，它们的既有
	// 判空逻辑因此会正确地拒绝——不会出现「用 API Key 把自己登出」这种操作。
	authMW := auth.NewMiddleware(deps.Auth).WithAPIKey(deps.APIKey)
	// 一次性动作令牌（G-53）：让下载直链可以通过 URL 里的 action_token
	// 完成认证。nil 时不启用（与 API 凭证同一模式）。
	if deps.APIKey != nil {
		authMW = authMW.WithActionToken(deps.APIKey)
	}
	// 认证接口都计为真实用户活动：它们由用户显式操作触发，不是后台轮询。
	g := guards{
		requireAuth: authMW.Require(auth.Real),
		adminOnly:   authz.Admin(),
	}

	v1 := h.Group("/api/v1")
	registerPlatformRoutes(v1, deps, g)
	registerNodeRoutes(v1, deps, g)
	registerComputeRoutes(v1, deps, g)
	registerNetworkRoutes(v1, deps, g)
	registerStorageRoutes(v1, deps, g)
	registerOpsRoutes(v1, deps, g)

	// 接口清单取自**已注册的路由**（NewAPIDocs 在构造时快照 h.Routes()），
	// 因此必须在全部路由注册完成之后构造——放在这里而不是任何域文件里，
	// 靠调用位置表达这条顺序约束。
	apiDocsHandler := platform.NewAPIDocs(h)
	v1.GET("/api-endpoints", g.requireAuth, apiDocsHandler.List)
}
