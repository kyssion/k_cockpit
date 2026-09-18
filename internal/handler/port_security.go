package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/portsecurity"
)

// PortSecurity 提供端口安全接口（F-4-08）。归管理员。
//
// 归管理员：它会改虚拟机所在二层网段的连通性——一个租户给自己开端口隔离，
// 影响的是**同网段所有机器**（包括别人的）。而"同网段"这件事在界面上看不
// 出来，租户无法判断自己会波及谁。
type PortSecurity struct {
	svc *portsecurity.Service
}

// NewPortSecurity 构造接口。
func NewPortSecurity(svc *portsecurity.Service) *PortSecurity {
	return &PortSecurity{svc: svc}
}

// List 返回节点上的策略（API-250）。
func (h *PortSecurity) List(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.List(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type portSecurityRequest struct {
	PortRef       string `json:"port_ref"`
	SwitchID      int64  `json:"switch_id"`
	VMID          int64  `json:"vm_id"`
	SpoofingGuard bool   `json:"spoofing_guard"`
	Isolation     bool   `json:"isolation"`
	PPSLimit      int    `json:"pps_limit"`
}

// Preview 预检（API-251）。**只读**。
func (h *PortSecurity) Preview(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req portSecurityRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	check, err := h.svc.Preview(ctx, toPSRequest(nodeID, req))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, check)
}

// Apply 配置并启用（API-252）。
//
// 有警告而未确认时**不执行、也不报错**，而是把预检结果原样返回——与防火墙、
// 端口镜像、存储卷同一套语义。
func (h *PortSecurity) Apply(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req struct {
		portSecurityRequest
		Acknowledge bool `json:"acknowledge"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	check, policy, taskID, err := h.svc.Apply(
		ctx, toPSRequest(nodeID, req.portSecurityRequest), req.Acknowledge,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"precheck": check, "policy": policy, "task_id": taskID})
}

// Disable 停用（API-253）。
//
// 不需要二次验证：策略关掉之后同网段立刻恢复互通，而这种变化是立刻可见的。
func (h *PortSecurity) Disable(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "策略 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	taskID, err := h.svc.Disable(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": taskID})
}

func toPSRequest(nodeID int64, r portSecurityRequest) portsecurity.Request {
	return portsecurity.Request{
		NodeID: nodeID, PortRef: r.PortRef, SwitchID: r.SwitchID, VMID: r.VMID,
		SpoofingGuard: r.SpoofingGuard, Isolation: r.Isolation, PPSLimit: r.PPSLimit,
	}
}
