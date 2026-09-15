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
	"k_cockpit/internal/node"
	"k_cockpit/internal/task"
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
	Task      *task.Queue

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

	authHandler := handler.NewAuth(deps.Auth, deps.SecureCookie)
	setupHandler := handler.NewSetup(deps.Bootstrap, deps.Auth, deps.SecureCookie)
	nodeHandler := handler.NewNode(deps.Node, deps.SimulateAgent)
	vmHandler := handler.NewVM(deps.VM)
	taskHandler := handler.NewTask(deps.Task)

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

		// 节点管理：按 f-1-06 的角色表，全部仅管理员可访问。
		v1.GET("/nodes", requireAuth, adminOnly, nodeHandler.List)
		v1.GET("/nodes/:id", requireAuth, adminOnly, nodeHandler.Get)
		v1.POST("/nodes/registration-tokens", requireAuth, adminOnly, nodeHandler.CreateEnrollToken)
		v1.DELETE("/nodes/:id", requireAuth, adminOnly, nodeHandler.Remove)

		// 虚拟机：管理员可操作全部，tenant 仅自己名下（归属过滤在数据访问层注入，
		// 因此这里不需要按角色分路由）。
		v1.GET("/vms", requireAuth, vmHandler.List)
		v1.GET("/vms/:id", requireAuth, vmHandler.Get)
		v1.POST("/vms", requireAuth, vmHandler.Create)
		// 电源与删除都是异步操作：受理时校验状态并返回任务标识，执行由
		// 任务队列按资源锁串行（f-2-01 R-005）。
		v1.POST("/vms/:id/power-actions", requireAuth, vmHandler.Power)
		v1.DELETE("/vms/:id", requireAuth, vmHandler.Delete)

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
