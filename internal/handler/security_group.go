package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/securitygroup"
)

// SecurityGroup 提供安全组接口（F-4-03 / F-4-04）。
//
// 权限：管理员管预置组与全部租户的组，租户管自己的。归属校验在服务层
// 统一处理（非所有者返回 404 而非 403，避免通过枚举确认 ID 存在）。
type SecurityGroup struct {
	svc *securitygroup.Service
}

// NewSecurityGroup 构造安全组接口。
func NewSecurityGroup(svc *securitygroup.Service) *SecurityGroup {
	return &SecurityGroup{svc: svc}
}

// ListGroups 返回安全组列表（API-110）。
func (h *SecurityGroup) ListGroups(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.ListGroups(ctx, int64(queryInt(c, "node_id")), authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type createGroupRequest struct {
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	Remark string `json:"remark"`
	// OwnerID 为空且调用者是管理员时，创建的是系统预置组。
	OwnerID *int64 `json:"owner_id"`
}

// CreateGroup 创建安全组（API-111）。
func (h *SecurityGroup) CreateGroup(ctx context.Context, c *app.RequestContext) {
	var req createGroupRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreateGroup(ctx, securitygroup.CreateGroupRequest{
		NodeID: req.NodeID, Name: req.Name, Remark: req.Remark, OwnerID: req.OwnerID,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DeleteGroup 删除安全组（API-112）。
//
// 仍被网口挂载时拒绝：删掉一个正在被使用的组会**静默地**放开一批机器的
// 流量——那是一次方向与用户预期相反的变更，而且不报任何错。
func (h *SecurityGroup) DeleteGroup(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "安全组 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.DeleteGroup(ctx, id, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

// ListRules 返回组内规则（API-113）。
func (h *SecurityGroup) ListRules(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "安全组 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.ListRules(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type ruleRequest struct {
	Direction   string `json:"direction"`
	Protocol    string `json:"protocol"`
	PortStart   *int   `json:"port_start"`
	PortEnd     *int   `json:"port_end"`
	TargetType  string `json:"target_type"`
	TargetValue string `json:"target_value"`
	Priority    int    `json:"priority"`
	Remark      string `json:"remark"`
}

func (r ruleRequest) toService() securitygroup.RuleRequest {
	return securitygroup.RuleRequest{
		Direction: r.Direction, Protocol: r.Protocol,
		PortStart: r.PortStart, PortEnd: r.PortEnd,
		TargetType: r.TargetType, TargetValue: r.TargetValue,
		Priority: r.Priority, Remark: r.Remark,
	}
}

// CreateRule 新增规则（API-114）。
func (h *SecurityGroup) CreateRule(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "安全组 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req ruleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreateRule(ctx, id, req.toService(),
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// UpdateRule 修改规则（API-115）。
func (h *SecurityGroup) UpdateRule(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "安全组 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	ruleID, err := namedPathID(c, "ruleID", "规则 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req ruleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.UpdateRule(ctx, id, ruleID, req.toService(),
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DeleteRule 删除规则（API-116）。
func (h *SecurityGroup) DeleteRule(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "安全组 ID")
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

	if err := h.svc.DeleteRule(ctx, id, ruleID,
		authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

// Attach 把安全组挂到网口（API-117）。
func (h *SecurityGroup) Attach(ctx context.Context, c *app.RequestContext) {
	ifaceID, err := namedPathID(c, "interfaceID", "网口 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	groupID, err := namedPathID(c, "id", "安全组 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Attach(ctx, ifaceID, groupID,
		authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"attached": true})
}

// Detach 解除挂载（API-118）。主组不能从这里解除。
func (h *SecurityGroup) Detach(ctx context.Context, c *app.RequestContext) {
	ifaceID, err := namedPathID(c, "interfaceID", "网口 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	groupID, err := namedPathID(c, "id", "安全组 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Detach(ctx, ifaceID, groupID,
		authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"detached": true})
}

// Effective 汇总一台虚拟机的生效规则（API-119）。
//
// 这是 F-4-04 的「汇总并预览」：把挂载的多个组的规则合并去重，并标出每条
// 规则来自哪些组——否则用户看到合并结果后不知道该去哪儿改。
func (h *SecurityGroup) Effective(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	preview, err := h.svc.Effective(ctx, vmID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, preview)
}

// Apply 应用生效规则（API-120）。
//
// **必须带回预览时的版本号**。用户看到预览、判断没问题、然后点确认，这中间
// 可能有几十秒，而别人完全可能改了其中一个组。不校验版本的话，用户批准的
// 是 A，落下去的是 B——这类问题不报错，只会让某天出现一条谁也想不起来
// 什么时候加的放行规则。
func (h *SecurityGroup) Apply(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req struct {
		Version string `json:"version"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Apply(ctx, vmID, req.Version,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
