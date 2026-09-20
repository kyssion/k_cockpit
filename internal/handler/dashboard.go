package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/dashboard"
)

// Dashboard 提供工作台概览（F-8-03 / F-8-04）。
type Dashboard struct {
	svc *dashboard.Service
}

// NewDashboard 构造接口。
func NewDashboard(svc *dashboard.Service) *Dashboard { return &Dashboard{svc: svc} }

// Summary 返回工作台的一次快照。
//
// 同一接口按角色返回不同范围（管理员看全景、租户看自己的），而不是两个
// 地址：工作台的**页面结构**对两种角色是同一套，不同的只是数字的范围。
// 分成两个接口会让「租户版少了什么」变成两处各写一遍的判断，而那正是
// 容易漏掉归属过滤的地方。
func (h *Dashboard) Summary(ctx context.Context, c *app.RequestContext) {
	summary, err := h.svc.Summary(ctx, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, summary)
}
