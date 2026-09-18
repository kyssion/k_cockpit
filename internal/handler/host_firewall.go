package handler

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/hostfirewall"
)

// HostFirewall 提供宿主机防火墙接口（F-4-11 第一层）。归管理员。
//
// **它与 KVM 网络防火墙是两套**：这一套保护的是宿主机自己与面板（SSH、
// 面板端口），而 /firewall/* 那一套管的是虚拟机的入站流量。归管理员的理由
// 很直接——它管的是"谁能登进这台机器"，而租户本来就不该有这台机器的登录权。
type HostFirewall struct {
	svc *hostfirewall.Service
}

// NewHostFirewall 构造接口。
func NewHostFirewall(svc *hostfirewall.Service) *HostFirewall {
	return &HostFirewall{svc: svc}
}

// Get 返回策略与规则（API-310）。
func (h *HostFirewall) Get(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	policy, rules, err := h.svc.GetPolicy(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"policy": policy, "rules": rules})
}

type hostPolicyRequest struct {
	Enabled       *bool   `json:"enabled"`
	DefaultAction string  `json:"default_action"`
	Whitelist     *string `json:"whitelist"`
}

// UpdatePolicy 更新策略（API-311）。
func (h *HostFirewall) UpdatePolicy(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req hostPolicyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	pv, err := h.svc.UpdatePolicy(ctx, nodeID, hostfirewall.PolicyRequest{
		Enabled: req.Enabled, DefaultAction: req.DefaultAction, Whitelist: req.Whitelist,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, pv)
}

type hostRuleRequest struct {
	Action       string   `json:"action"`
	Protocol     string   `json:"protocol"`
	PortStart    *int     `json:"port_start"`
	PortEnd      *int     `json:"port_end"`
	SourceCIDR   *string  `json:"source_cidr"`
	GeoipRegions []string `json:"geoip_regions"`
	Remark       string   `json:"remark"`
}

// CreateRule 新建规则（API-312）。
func (h *HostFirewall) CreateRule(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req hostRuleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreateRule(ctx, hostfirewall.RuleRequest{
		NodeID: nodeID, Action: req.Action, Protocol: req.Protocol,
		PortStart: req.PortStart, PortEnd: req.PortEnd,
		SourceCIDR: req.SourceCIDR, GeoipRegions: req.GeoipRegions, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DeleteRule 删除规则（API-313）。
//
// 保护规则**服务端强制拒绝**，不看界面传了什么——让界面决定能不能删，
// 等于把一个不可恢复的操作交给一次点击。
func (h *HostFirewall) DeleteRule(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	ruleID, err := namedPathID(c, "id", "规则 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.DeleteRule(ctx, nodeID, ruleID, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}

// Precheck 预览将要下发的规则（API-314）。**只读**。
func (h *HostFirewall) Precheck(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	preview, warnings, err := h.svc.Precheck(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"preview": preview, "warnings": warnings})
}

// Apply 应用（API-315）。
//
// **必须带回预览时的 version**：用户看到预览、判断没问题、点确认，中间
// 别人完全可能改了策略。不校验的话，他批准的是 A、落下去的是 B。
func (h *HostFirewall) Apply(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req struct {
		Version int64 `json:"version"`
	}
	_ = c.Bind(&req)

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Apply(ctx, nodeID, req.Version, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// Rollback 紧急回滚（API-316）。
//
// **不校验版本、不要求二次验证**：它要在"已经出事了"的那一刻还能用——
// 应用之后连不上面板或 SSH 断了，而用户又进不去宿主机时，这是唯一的自救
// 入口。把它做得难用，等于在最需要它的时候把它关掉。
func (h *HostFirewall) Rollback(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Rollback(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// Connections 返回当前连接（API-317）。
func (h *HostFirewall) Connections(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	conns, err := h.svc.Connections(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 标记"哪条是你自己那条"。
	//
	// 节点无从知道请求方是谁，因此这一步只能在控制面做——而它很有必要：
	// 关掉自己那条连接会让人以为面板挂了，界面上必须能提前看出来。
	caller := auth.ClientInfoOf(c).IP
	for i := range conns {
		if caller != "" && strings.HasPrefix(conns[i].RemoteAddr, caller+":") {
			conns[i].Own = true
		}
	}
	api.OK(c, map[string]any{"items": conns})
}

// CloseConnection 关闭一条连接（API-318）。
func (h *HostFirewall) CloseConnection(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req struct {
		RemoteAddr string `json:"remote_addr"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.CloseConnection(ctx, nodeID, req.RemoteAddr,
		authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}
