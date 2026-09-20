package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/quotaenforce"
)

// QuotaEnforce 提供资源配额接口（F-4-10）。归管理员。
//
// 归管理员：配额是**跨用户的资源分配**，一个租户给自己调额度等于没有配额。
type QuotaEnforce struct {
	svc *quotaenforce.Service
}

// NewQuotaEnforce 构造接口。
func NewQuotaEnforce(svc *quotaenforce.Service) *QuotaEnforce {
	return &QuotaEnforce{svc: svc}
}

// List 返回节点上的配额与当前用量（API-300）。
func (h *QuotaEnforce) List(ctx context.Context, c *app.RequestContext) {
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

type quotaRequest struct {
	UserID     int64  `json:"user_id"`
	Dimension  string `json:"dimension"`
	LimitValue int64  `json:"limit_value"`
	Action     string `json:"action"`
}

// Set 设置配额（API-301）。
func (h *QuotaEnforce) Set(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req quotaRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Set(ctx, quotaenforce.Request{
		NodeID: nodeID, UserID: req.UserID, Dimension: req.Dimension,
		LimitValue: req.LimitValue, Action: req.Action,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// ResetUsage 清空配额在本周期的累计用量（F-1-09）。
//
// 与"改上限"不同：那是把上限调高（累计仍然算数），这是把累计清零。界面上
// 必须把它标成"重置用量"，否则管理员点完发现五分钟后又被限速，会以为
// 功能坏了。
func (h *QuotaEnforce) ResetUsage(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "配额 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.ResetUsage(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Delete 删除配额（API-302）。
func (h *QuotaEnforce) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "配额 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Delete(ctx, id, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}
