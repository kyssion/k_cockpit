package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/accesscontrol"
	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
)

// AccessControl 提供公网访问与开发模式开关（F-10-06）。归管理员。
type AccessControl struct {
	svc *accesscontrol.Service
}

// NewAccessControl 构造接口。
func NewAccessControl(svc *accesscontrol.Service) *AccessControl {
	return &AccessControl{svc: svc}
}

// Get 返回当前状态（API-350）。
func (h *AccessControl) Get(ctx context.Context, c *app.RequestContext) {
	info := auth.ClientInfoOf(c)
	v, err := h.svc.Get(ctx, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, v)
}

// Set 修改开关（API-351）。
//
// **关掉公网访问而调用方自己就在公网上时**，需要显式确认——那一刻他的连接
// 会断，而界面还没来得及显示"已保存"。
func (h *AccessControl) Set(ctx context.Context, c *app.RequestContext) {
	var req struct {
		PublicEnabled *bool `json:"public_enabled"`
		DevMode       *bool `json:"dev_mode"`
		Confirm       bool  `json:"confirm"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	v, err := h.svc.Set(ctx, accesscontrol.Request{
		PublicEnabled: req.PublicEnabled, DevMode: req.DevMode,
	}, req.Confirm, info.IP, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, v)
}
