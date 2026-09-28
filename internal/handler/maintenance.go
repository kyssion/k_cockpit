package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/maintenance"
	"k_cockpit/internal/risk"
)

// Maintenance 提供站点维护模式（G-46）。归管理员。
type Maintenance struct {
	svc  *maintenance.Service
	risk *risk.Guard
}

// NewMaintenance 构造接口。
func NewMaintenance(svc *maintenance.Service, guard *risk.Guard) *Maintenance {
	return &Maintenance{svc: svc, risk: guard}
}

// Status 返回站点维护状态。
func (h *Maintenance) Status(ctx context.Context, c *app.RequestContext) {
	view, err := h.svc.Status(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Enter 进入站点维护（逐节点接管，可选批量关机）。
func (h *Maintenance) Enter(ctx context.Context, c *app.RequestContext) {
	var req maintenance.EnterRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	// 影响整个站点的可达性，必须过二次验证；与其它写接口一致，验证通过后
	// 这里拿到的是完整会话视角。
	if !h.risk.Require(c, risk.ActionSiteMaintenanceEnter) {
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)
	view, err := h.svc.Enter(ctx, req, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Exit 退出站点维护（只解除本次接管的节点）。
func (h *Maintenance) Exit(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)
	view, err := h.svc.Exit(ctx, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}
