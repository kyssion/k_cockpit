// routes_node 登记节点接入与投影域的路由：节点列表与详情、宿主机指标、
// 维护模式开关、注册令牌与节点移除，以及开发期的模拟注册入口。
package router

import (
	"github.com/cloudwego/hertz/pkg/route"

	nodehandler "k_cockpit/internal/handler/node"
)

func registerNodeRoutes(v1 *route.RouterGroup, deps Deps, g guards) {
	nodeHandler := nodehandler.NewNode(deps.Node, deps.SimulateAgent, deps.Risk)

	{
		// 节点管理：按 f-1-06 的角色表，全部仅管理员可访问。
		v1.GET("/nodes", g.requireAuth, g.adminOnly, nodeHandler.List)
		v1.GET("/nodes/:id", g.requireAuth, g.adminOnly, nodeHandler.Get)
		// 宿主机指标（F-6-03）。只读探测，不入队、不写投影——指标是瞬时的，
		// 存下来只会在下一次读取时给出一个过期的答案。
		v1.GET("/nodes/:id/stats", g.requireAuth, g.adminOnly, nodeHandler.Stats)

		// 维护模式（F-6-05 / API-042）。**同步生效，不进任务队列**——
		// 它纯粹是控制面的标志，所有拦截都发生在受理那一刻；做成任务会
		// 制造一个「界面说维护中、操作仍被受理」的窗口。
		v1.PATCH("/nodes/:id/maintenance", g.requireAuth, g.adminOnly, nodeHandler.SetMaintenance)
		// 控制台对外地址：决定能否生成 SPICE 连接文件。
		v1.PATCH("/nodes/:id/console-host", g.requireAuth, g.adminOnly, nodeHandler.SetConsoleHost)

		v1.POST("/nodes/registration-tokens", g.requireAuth, g.adminOnly, nodeHandler.CreateEnrollToken)
		v1.DELETE("/nodes/:id", g.requireAuth, g.adminOnly, nodeHandler.Remove)

		if deps.SimulateAgent {
			// 开发期专用：调用它与节点走**同一段注册逻辑**（ADR-0007）。
			// 它不需要认证——节点注册也不凭用户身份，而凭注册令牌。
			v1.POST("/dev/agent-register", nodeHandler.SimulateRegister)
		}
	}
}
