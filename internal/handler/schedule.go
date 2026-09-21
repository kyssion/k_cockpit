package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/schedule"
)

// Schedule 提供虚拟机的定时任务接口（F-7-05）。
type Schedule struct {
	svc  *schedule.Service
	risk *risk.Guard
}

// NewSchedule 构造定时任务接口。
func NewSchedule(svc *schedule.Service, guard *risk.Guard) *Schedule {
	return &Schedule{svc: svc, risk: guard}
}

type createScheduleRequest struct {
	// Action 取值 start / shutdown / delete / snapshot。
	Action string `json:"action"`
	// SnapshotName 是快照名模板，仅 snapshot 动作需要：它让自动快照在列表里能与手动快照区分。
	SnapshotName string `json:"snapshot_name"`
	// IncludeMemory 是否保存运行现场，仅 snapshot 动作使用。
	IncludeMemory bool `json:"include_memory"`
	// ScheduleType 取值 once / daily / weekly。
	ScheduleType string `json:"schedule_type"`
	// Weekdays 每周模式下要执行的日子，1=周一 … 7=周日。
	Weekdays []int `json:"weekdays"`
	// TimeOfDay 执行时刻，形如 `03:00`。
	TimeOfDay string `json:"time_of_day"`
	// Date 一次性任务的日期，形如 `2026-09-20`。
	Date string `json:"date"`
}

// List 返回某台虚拟机的定时任务（API-048）。
func (h *Schedule) List(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.List(ctx, vmID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Create 新建定时任务（API-049）。
//
// 删除类任务**在创建时**完成二次验证：它是一条将来会自动执行的删除指令，
// 留到执行时再验证的话，那一刻没有人在场，验证也就无从谈起。
func (h *Schedule) Create(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req createScheduleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	// 判断放在解析之后：只有删除类需要验证，而这一点要先知道动作才能确定。
	if req.Action == model.ScheduleActionDelete {
		if !h.risk.Require(c, risk.ActionVMDelete) {
			return
		}
	}

	row, err := h.svc.Create(ctx, vmID, schedule.CreateRequest{
		Action:        req.Action,
		ScheduleType:  req.ScheduleType,
		Weekdays:      req.Weekdays,
		TimeOfDay:     req.TimeOfDay,
		Date:          req.Date,
		SnapshotName:  req.SnapshotName,
		IncludeMemory: req.IncludeMemory,
	}, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, row)
}

type setScheduleEnabledRequest struct {
	Enabled *bool `json:"enabled"`
}

// SetEnabled 启用或停用定时任务（API-050）。
//
// **不是删除**：停用保留记录与执行历史，用户能看出「它跑过」；删除则让这条
// 痕迹消失。界面因此把两者分开，而不是用一个「开关式的删除」。
func (h *Schedule) SetEnabled(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	scheduleID, err := namedPathID(c, "scheduleID", "定时任务 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req setScheduleEnabledRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	// 显式要求 enabled 字段：用 bool 零值会让「没传」与「传了 false」
	// 无法区分，而后者是用户明确要关闭任务，不能当成参数缺失忽略掉。
	if req.Enabled == nil {
		api.Fail(c, api.InvalidParameter("缺少 enabled 字段"))
		return
	}

	row, err := h.svc.SetEnabled(ctx, vmID, scheduleID, *req.Enabled, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, row)
}

// Delete 删除定时任务定义（API-051）。
//
// **删除的是任务定义，不是虚拟机**——这一点必须在文案上说清楚，否则用户
// 会因为「这里能删东西」而误解。因此它不需要二次验证：可逆性完全不同。
func (h *Schedule) Delete(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	scheduleID, err := namedPathID(c, "scheduleID", "定时任务 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	if err := h.svc.Delete(ctx, vmID, scheduleID, authz.ViewerOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}
