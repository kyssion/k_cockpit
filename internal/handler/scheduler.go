package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/scheduler"
)

// Scheduler 提供调度器与调度事件查询（F-7-04）。整体归管理员。
//
// 归管理员的理由：这里暴露的是**系统内部的运行细节**（有哪些周期任务、
// 它们最近做了什么、哪些失败了）。它对运维排查有用，而对租户没有意义——
// 租户关心的是自己那台机器，不是宿主机上跑着几个后台任务。
type Scheduler struct {
	svc *scheduler.Service
}

// NewScheduler 构造接口。
func NewScheduler(svc *scheduler.Service) *Scheduler { return &Scheduler{svc: svc} }

// Overview 返回全部调度器及各自的最近事件（API-240）。
func (h *Scheduler) Overview(ctx context.Context, c *app.RequestContext) {
	per := queryInt(c, "per_scheduler")
	items, err := h.svc.Overview(ctx, per)
	if err != nil {
		api.Fail(c, api.Internal())
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Events 返回事件流（API-241）。
func (h *Scheduler) Events(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.Events(ctx, c.Query("scheduler_key"), c.Query("status"), queryInt(c, "limit"))
	if err != nil {
		api.Fail(c, api.Internal())
		return
	}
	api.OK(c, map[string]any{"items": items})
}
