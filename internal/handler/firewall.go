package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/firewall"
)

// Firewall 提供防火墙接口（F-4-11）。
//
// 权限：整体归 admin。防火墙作用于宿主机，一次配错会影响该节点上的所有
// 虚拟机，而且**可能把管理员自己关在门外**——那不是一个租户该有的能力。
type Firewall struct {
	svc *firewall.Service
}

// NewFirewall 构造防火墙接口。
func NewFirewall(svc *firewall.Service) *Firewall {
	return &Firewall{svc: svc}
}

func nodeIDOf(c *app.RequestContext) (int64, error) {
	id := int64(queryInt(c, "node_id"))
	if id <= 0 {
		return 0, api.InvalidParameter("必须指定 node_id")
	}
	return id, nil
}

// GetPolicy 返回节点的防火墙策略（API-150）。
func (h *Firewall) GetPolicy(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	view, err := h.svc.GetPolicy(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type updatePolicyRequest struct {
	Enabled       *bool    `json:"enabled"`
	DefaultAction string   `json:"default_action"`
	GeoipRegions  []string `json:"geoip_regions"`
	Whitelist     []string `json:"whitelist"`
}

// UpdatePolicy 更新节点策略（API-151）。
//
// **只改配置，不下发**。把两者分开是因为下发有真实的网络影响，而改配置
// 不该有——合成一步会让「我想先看看改完是什么样」变得不可能。
func (h *Firewall) UpdatePolicy(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req updatePolicyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.UpdatePolicy(ctx, nodeID, firewall.UpdatePolicyRequest{
		Enabled: req.Enabled, DefaultAction: req.DefaultAction,
		GeoipRegions: req.GeoipRegions, Whitelist: req.Whitelist,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// ListRules 返回节点级规则（API-152）。
func (h *Firewall) ListRules(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.ListRules(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type firewallRuleRequest struct {
	Action     string `json:"action"`
	Protocol   string `json:"protocol"`
	PortStart  *int   `json:"port_start"`
	PortEnd    *int   `json:"port_end"`
	SourceCIDR string `json:"source_cidr"`
	OrderNo    int    `json:"order_no"`
	Remark     string `json:"remark"`
}

// CreateRule 新增规则（API-153）。
func (h *Firewall) CreateRule(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req firewallRuleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreateRule(ctx, nodeID, firewall.RuleRequest{
		Action: req.Action, Protocol: req.Protocol,
		PortStart: req.PortStart, PortEnd: req.PortEnd,
		SourceCIDR: req.SourceCIDR, OrderNo: req.OrderNo, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DeleteRule 删除规则（API-154）。
//
// **保护规则拒绝删除**，且由**服务端**强制：一次「清理规则」的操作就能把
// SSH 或面板端口关掉，而那种事故无法通过面板恢复——那时已经连不上了。
func (h *Firewall) DeleteRule(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	ruleID, err := namedPathID(c, "ruleID", "规则 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.DeleteRule(ctx, nodeID, ruleID,
		authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

// GetVMPolicy 返回一台虚拟机实际受到的约束（API-155）。
func (h *Firewall) GetVMPolicy(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	view, err := h.svc.Effective(ctx, vmID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type setVMPolicyRequest struct {
	Action    string `json:"action"`
	Whitelist string `json:"whitelist"`
	// Regions 是**替换**节点级的区域限制，而不是叠加——覆盖层里不填
	// 就意味着这台机器不做区域限制。
	Regions []string `json:"geoip_regions"`
	// Enabled 用指针区分「没传」与「传了 false」。
	Enabled *bool `json:"enabled"`
}

// SetVMPolicy 设置虚拟机覆盖策略（API-156）。
func (h *Firewall) SetVMPolicy(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req setVMPolicyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.SetVMPolicy(ctx, vmID, req.Action, req.Whitelist, req.Regions, enabled,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// ClearVMPolicy 移除覆盖、回落到节点基线（API-157）。
func (h *Firewall) ClearVMPolicy(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.ClearVMPolicy(ctx, vmID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Precheck 预检一次待应用的改动（API-158）。
func (h *Firewall) Precheck(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	info := auth.ClientInfoOf(c)

	warnings, err := h.svc.Precheck(ctx, nodeID, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"warnings": warnings})
}

type applyFirewallRequest struct {
	// Acknowledge 表示调用方**已经看过**预检警告并确认继续。
	//
	// 有警告而未确认时服务端**不下发、也不报错**，而是把警告原样返回。
	// 报错会让界面把它显示成一次失败，而它实际是一个需要用户做决定的
	// 岔路口——两者的界面完全不同。
	Acknowledge bool `json:"acknowledge"`
}

// Apply 下发策略（API-159）。
func (h *Firewall) Apply(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req applyFirewallRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.Apply(ctx, nodeID, req.Acknowledge, info.IP,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// Rollback 紧急回滚（API-160）。
//
// **不做任何确认、不做预检、不需要二次验证**，一键关闭。
//
// 这个「不做」是有意的**不对称**：通往事故的路要设卡（Apply 要确认警告），
// 从事故里出来的路不能设卡。一个被自己配错的防火墙关在门外的管理员，此刻
// 唯一的诉求是「先让我进去」，而任何一道额外确认都会成为压垮他的那一步。
func (h *Firewall) Rollback(ctx context.Context, c *app.RequestContext) {
	nodeID, err := nodeIDOf(c)
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.Rollback(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}
