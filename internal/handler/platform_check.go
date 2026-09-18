package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/platformcheck"
)

// PlatformCheck 提供平台自检与修复接口（F-4-13）。归管理员。
//
// 归管理员的理由：自检结果里含宿主机环境、全部网络配置的期望与实际状态；
// 修复则是一个会改宿主机网络的写操作。
type PlatformCheck struct {
	svc *platformcheck.Service
}

// NewPlatformCheck 构造接口。
func NewPlatformCheck(svc *platformcheck.Service) *PlatformCheck {
	return &PlatformCheck{svc: svc}
}

// ClientIP 返回请求方地址（API-340）。
//
// 它的用途很具体：**宿主机防火墙的白名单**。管理员要填自己的地址，而
// 「我的 IP 是多少」在浏览器里看不到。
func (h *PlatformCheck) ClientIP(_ context.Context, c *app.RequestContext) {
	info := auth.ClientInfoOf(c)
	api.OK(c, platformcheck.ClientIP(info.IP, string(c.GetHeader("X-Forwarded-For"))))
}

// OVSStatus 返回 OVS 状态（API-341）。
func (h *PlatformCheck) OVSStatus(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	st, err := h.svc.OVSStatus(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, st)
}

// OVSPorts 返回端口列表（API-342）。
func (h *PlatformCheck) OVSPorts(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.OVSPorts(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Leases 返回 DHCP 租约（API-343）。
func (h *PlatformCheck) Leases(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.Leases(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Check 执行自检（API-344）。
//
// 它返回的是**偏差清单**而不只是「能力齐不齐」：面板显示「端口安全已启用」
// 而节点上的流表早被一次重启清掉了——这个状态不会以任何形式报警，自检正是
// 去找它。
func (h *PlatformCheck) Check(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	result, err := h.svc.Check(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// Repair 按期望状态重新下发（API-345）。
func (h *PlatformCheck) Repair(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req struct {
		Kinds []string `json:"kinds"`
	}
	_ = c.Bind(&req)
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Repair(ctx, nodeID, req.Kinds, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}
