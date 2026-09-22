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

// HostDetail 返回某个节点的宿主机细节（KSM / zRAM / 硬件 / 网络统计）。
//
// 单独一个接口而不是塞进 Summary：Summary 是"打开首页就要的"，而这些
// 细节需要向节点发请求。混在一起会让首页在节点多时变慢，并且在某个节点
// 不支持探测时把整页拖成错误。
func (h *Dashboard) HostDetail(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	detail, err := h.svc.HostDetail(ctx, nodeID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, detail)
}

// MyQuotas 返回当前用户的配额总览（G-32）。
//
// 单独一个接口而不是塞进 Summary：配额由三个可选服务拼装，任何一个缺失
// 都不该拖慢或拖垮首页；而普通用户工作台的配额卡是这个接口唯一的使用方。
// 管理员调用返回空列表（而不是 403）：他的工作台走平台视图，前端不必为
// 同一页面维护两条错误路径。
func (h *Dashboard) MyQuotas(ctx context.Context, c *app.RequestContext) {
	overview, err := h.svc.QuotaOverview(ctx, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, overview)
}
