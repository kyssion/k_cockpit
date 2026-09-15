package handler

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/node"
	"k_cockpit/internal/risk"
)

// Node 提供节点管理接口（F-6-01 / F-6-02）。
//
// 全部接口都要求管理员角色——该要求由**路由注册时声明**，而不是在
// 本文件内部判断（f-1-06 R-003，见 internal/router）。
type Node struct {
	svc *node.Service
	// simulateAgent 为 true 时暴露开发期的模拟注册入口。
	// 仅在 AGENT_TRANSPORT=mock 时启用；接入真实 agent 后该路由不再注册。
	simulateAgent bool
	risk          *risk.Guard
}

// NewNode 构造节点接口。
func NewNode(svc *node.Service, simulateAgent bool, guard *risk.Guard) *Node {
	return &Node{svc: svc, simulateAgent: simulateAgent, risk: guard}
}

// List 返回节点列表（含运行态）。
func (h *Node) List(ctx context.Context, c *app.RequestContext) {
	views, err := h.svc.List(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, views)
}

// Get 返回节点详情。
func (h *Node) Get(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	view, err := h.svc.Get(ctx, id)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Remove 移除节点。
func (h *Node) Remove(ctx context.Context, c *app.RequestContext) {
	// 高风险操作：移除节点后其上的虚拟机将失去管控（f-10-02）。
	if !h.risk.Require(c, risk.ActionNodeRemove) {
		return
	}

	id, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)
	if err := h.svc.Remove(ctx, id, user.ID, user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

type createEnrollTokenRequest struct {
	Name string `json:"name"`
	// TTLHours 为 0 时使用默认有效期（24 小时）。
	TTLHours int `json:"ttl_hours"`
}

type createEnrollTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Node      node.View `json:"node"`

	// InstallCommand 是在目标机器上执行的 agent 安装命令。
	// agent 尚未开发，接入后填充（见 docs/06-decisions/0007-mock-agent-first.md）。
	InstallCommand string `json:"install_command"`
	// SimulateCommand 是开发期的模拟注册命令：执行它会走与真实 agent
	// **相同的注册逻辑**，区别只是把「在目标机器上执行」换成了直接调用。
	SimulateCommand string `json:"simulate_command,omitempty"`
}

// CreateEnrollToken 生成一次性注册令牌，并创建一条待接入的节点。
//
// 明文令牌只在本次响应中出现一次——数据库存的是它的哈希，之后无法取出。
func (h *Node) CreateEnrollToken(ctx context.Context, c *app.RequestContext) {
	var req createEnrollTokenRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	var ttl time.Duration
	if req.TTLHours > 0 {
		ttl = time.Duration(req.TTLHours) * time.Hour
	}

	result, err := h.svc.CreateEnrollToken(ctx, req.Name, ttl, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	resp := createEnrollTokenResponse{
		Token:     result.Token,
		ExpiresAt: result.ExpiresAt,
		Node:      result.Node,
	}

	if h.simulateAgent {
		resp.SimulateCommand = fmt.Sprintf(
			`curl -sS -X POST '%s/api/v1/dev/agent-register' `+
				`-H 'Content-Type: application/json' `+
				`-d '{"token":"%s","agent_id":"mock-agent-%d"}'`,
			baseURL(c), result.Token, result.Node.ID,
		)
	} else {
		resp.InstallCommand = "（agent 安装脚本尚未实现）"
	}

	api.Created(c, resp)
}

type simulateRegisterRequest struct {
	Token        string `json:"token"`
	AgentID      string `json:"agent_id"`
	AgentVersion string `json:"agent_version"`
}

// SimulateRegister 是**开发期**的模拟注册入口。
//
// 它调用与真实 agent **同一个** Register 方法（ADR-0007）：注册逻辑因此
// 是被真实验证过的，接入真实 agent 时无需改动本方法所依赖的任何代码。
// 该路由仅在 AGENT_TRANSPORT=mock 时注册，生产环境不存在。
func (h *Node) SimulateRegister(ctx context.Context, c *app.RequestContext) {
	var req simulateRegisterRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.AgentVersion == "" {
		req.AgentVersion = "mock-0.1.0"
	}

	view, err := h.svc.Register(ctx, req.Token, req.AgentID, req.AgentVersion)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// baseURL 由请求推导服务的基础地址，用于拼接给用户执行的命令。
//
// 取请求的 Host 而非配置里的 APP_BASE_URL：用户可能是通过 IP、域名或
// 反向代理访问的，用配置默认值拼出来的命令在他的环境里多半连不上。
func baseURL(c *app.RequestContext) string {
	scheme := "http"
	if proto := string(c.GetHeader("X-Forwarded-Proto")); proto == "https" {
		scheme = "https"
	}
	return scheme + "://" + string(c.Request.Host())
}
