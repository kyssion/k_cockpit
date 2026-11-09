package ops

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/schedule"
	"k_cockpit/internal/service/platform/risk"
)

// Schedule 提供虚拟机的定时任务接口（F-7-05）。
type Schedule struct {
	svc  *cron.Service
	risk *risk.Guard
}

// NewSchedule 构造定时任务接口。
func NewSchedule(svc *cron.Service, guard *risk.Guard) *Schedule {
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
	vmID, err := api.NamedPathID(c, "id", "虚拟机 ID")
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
	vmID, err := api.NamedPathID(c, "id", "虚拟机 ID")
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

	row, err := h.svc.Create(ctx, vmID, cron.CreateRequest{
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

// setScheduleEnabledRequest 是 PATCH 的请求体。
//
// 除 enabled 之外的字段**全部用指针接收**，而且一个都不能省略声明：本接口
// 只做启停，但调用方（尤其是把整张表单回传的界面）很容易顺带把执行时间、
// 动作一起发过来。如果这里不声明这些字段，多余字段会被**静默丢弃**——
// 接口返回 200，用户以为执行时间改了，实际只改了开关。把所有已知可编辑的
// 字段都显式接住，就能把「静默丢弃」变成一次明确的报错（见 SetEnabled）。
type setScheduleEnabledRequest struct {
	Enabled *bool `json:"enabled"`

	// --- 以下均为「已知可编辑、但本接口不支持」的字段（与 Create 取同一套全集）---

	Action        *string `json:"action"`
	SnapshotName  *string `json:"snapshot_name"`
	IncludeMemory *bool   `json:"include_memory"`
	ScheduleType  *string `json:"schedule_type"`
	Weekdays      *[]int  `json:"weekdays"`
	TimeOfDay     *string `json:"time_of_day"`
	Date          *string `json:"date"`
	// Cron / Name 不在 Create 的字段里，但界面上有过按 cron 表达式或名字
	// 提交的写法（旧客户端的残留），一并接住以免它们再次落入静默丢弃。
	Cron *string `json:"cron"`
	Name *string `json:"name"`
}

// unsupportedFields 返回请求里出现的、PATCH 不支持修改的字段名。
//
// 判据是**是否出现**而不是取值是否为空：传了 `"time_of_day": ""` 的调用方
// 同样以为自己改了什么，照样要告诉他改不了。
func (r setScheduleEnabledRequest) unsupportedFields() []string {
	var out []string
	if r.Action != nil {
		out = append(out, "action")
	}
	if r.SnapshotName != nil {
		out = append(out, "snapshot_name")
	}
	if r.IncludeMemory != nil {
		out = append(out, "include_memory")
	}
	if r.ScheduleType != nil {
		out = append(out, "schedule_type")
	}
	if r.Weekdays != nil {
		out = append(out, "weekdays")
	}
	if r.TimeOfDay != nil {
		out = append(out, "time_of_day")
	}
	if r.Date != nil {
		out = append(out, "date")
	}
	if r.Cron != nil {
		out = append(out, "cron")
	}
	if r.Name != nil {
		out = append(out, "name")
	}
	return out
}

// SetEnabled 启用或停用定时任务（API-050）。
//
// **不是删除**：停用保留记录与执行历史，用户能看出「它跑过」；删除则让这条
// 痕迹消失。界面因此把两者分开，而不是用一个「开关式的删除」。
func (h *Schedule) SetEnabled(ctx context.Context, c *app.RequestContext) {
	vmID, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	scheduleID, err := api.NamedPathID(c, "scheduleID", "定时任务 ID")
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

	// 带了本接口改不了的字段就**拒绝**，而不是收下 enabled、把其余的丢掉。
	//
	// 静默丢弃比报错更糟：请求返回 200，用户据此以为执行时间已经改了，直到
	// 某天发现任务还在老时间跑——那时他已经想不起是哪一步没生效。目前只有
	// GET/POST/PATCH/DELETE 四条路由，没有整条更新的接口，所以文案指向的
	// 补救办法是「删除后重建」，而不是一个并不存在的 PUT。
	if extra := req.unsupportedFields(); len(extra) > 0 {
		api.Fail(c, api.InvalidParameter(
			"PATCH 仅支持启停（enabled）；请求里还带了 "+strings.Join(extra, "、")+
				" —— 修改执行时间/动作请删除该任务后重新创建"))
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
	vmID, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	scheduleID, err := api.NamedPathID(c, "scheduleID", "定时任务 ID")
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
