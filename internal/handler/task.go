package handler

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/realtime"
	"k_cockpit/internal/task"
)

// Task 提供任务接口（F-7-02）。
type Task struct {
	queue *task.Queue
	// bus 提供实时通道；为 nil 时 Stream 返回"未启用"而不是假装成流。
	bus *realtime.Bus
}

// NewTask 构造任务接口。bus 可为 nil。
func NewTask(queue *task.Queue, bus *realtime.Bus) *Task {
	return &Task{queue: queue, bus: bus}
}

// taskView 是任务的对外视图。
//
// Params 与 Result 从 JSON 文本解析为对象再返回：前端不必再做一次解析，
// 也避免把「存储格式」当成「接口格式」泄漏出去。
type taskView struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	NodeID  *int64 `json:"node_id,omitempty"`
	OwnerID *int64 `json:"owner_id,omitempty"`

	ResourceType string `json:"resource_type,omitempty"`
	ResourceID   *int64 `json:"resource_id,omitempty"`
	ResourceName string `json:"resource_name,omitempty"`

	Progress     int    `json:"progress"`
	CurrentStage string `json:"current_stage,omitempty"`
	Error        string `json:"error,omitempty"`

	Params any `json:"params,omitempty"`
	Result any `json:"result,omitempty"`

	CancelRequested bool   `json:"cancel_requested"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	CreatedAt       string `json:"created_at"`

	// Stages 是阶段流水，**仅在详情接口返回**。
	//
	// 列表不带它：一页 20 个任务就是 20 次额外查询，而列表上根本显示不下
	// 一条时间线——它只会被白白查出来再丢掉。
	Stages []taskStageView `json:"stages,omitempty"`
}

// taskStageView 是任务阶段的对外视图。
type taskStageView struct {
	Seq  int    `json:"seq"`
	Key  string `json:"key"`
	Name string `json:"name"`
	// Status 取值与 task.status 同一套词汇（pending/running/success/failed/skipped）。
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	// DurationMS 是耗时毫秒。已在界面上做换算会让人疑惑「为什么没有秒」，
	// 而耗时超过一分钟的任务在这里会得到五位数——可读性由前端负责。
	DurationMS  int64  `json:"duration_ms"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
	Retryable   bool   `json:"retryable"`
	RetriedFrom *int64 `json:"retry_of_stage_id,omitempty"`
}

// List 返回任务列表（含归属过滤）。
func (h *Task) List(ctx context.Context, c *app.RequestContext) {
	page, pageSize := pageParams(c)

	items, total, err := h.queue.List(ctx, task.Filter{
		Status:     c.Query("status"),
		Type:       c.Query("type"),
		Page:       page,
		PageSize:   pageSize,
		Viewer:     authz.ViewerOf(c),
		ResourceID: int64(queryInt(c, "resource_id")),
	})
	if err != nil {
		api.Fail(c, err)
		return
	}

	views := make([]taskView, 0, len(items))
	for i := range items {
		views = append(views, toTaskView(&items[i]))
	}
	api.OKPage(c, views, api.NewPage(page, pageSize, total))
}

// Get 返回任务详情。
func (h *Task) Get(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "任务 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	t, err := h.queue.Get(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	view := toTaskView(t)

	// 归属校验已经在 Queue.Get 里完成，因此到这里才查阶段——反过来会先
	// 泄漏「这个任务存在且有多少个阶段」这一信息。
	stages, err := h.queue.Stages(ctx, id)
	if err != nil {
		// 查阶段失败不阻断详情：时间线是排查的辅助信息，让整个详情页报错
		// 会连任务的状态与结果都看不到，得不偿失。
		api.Fail(c, err)
		return
	}
	for i := range stages {
		view.Stages = append(view.Stages, toTaskStageView(&stages[i]))
	}

	api.OK(c, view)
}

func toTaskStageView(s *model.TaskStage) taskStageView {
	view := taskStageView{
		Seq:         s.Seq,
		Key:         s.Key,
		Name:        s.Name,
		Status:      s.Status,
		DurationMS:  s.Duration().Milliseconds(),
		Retryable:   s.Retryable,
		RetriedFrom: s.RetryOfStageID,
	}
	// Name 可能为空（旧数据或节点只给了 key），此时回落到 key——一个空的
	// 阶段名在时间线上就是一行空白，看起来像渲染坏了。
	if view.Name == "" {
		view.Name = s.Key
	}
	if s.Message != nil {
		view.Message = *s.Message
	}
	if s.StartedAt != nil {
		view.StartedAt = s.StartedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if s.FinishedAt != nil {
		view.FinishedAt = s.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

// Cancel 请求取消任务。
func (h *Task) Cancel(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "任务 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.queue.Cancel(ctx, id, authz.ViewerOf(c), user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, toTaskView(t))
}

func toTaskView(t *model.Task) taskView {
	view := taskView{
		ID:              t.ID,
		Type:            t.Type,
		Status:          t.Status,
		NodeID:          t.NodeID,
		OwnerID:         t.OwnerID,
		ResourceID:      t.ResourceID,
		Progress:        t.Progress,
		CancelRequested: t.CancelRequested,
		CreatedAt:       t.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if t.ResourceType != nil {
		view.ResourceType = *t.ResourceType
	}
	if t.ResourceName != nil {
		view.ResourceName = *t.ResourceName
	}
	if t.CurrentStage != nil {
		view.CurrentStage = *t.CurrentStage
	}
	if t.Error != nil {
		view.Error = *t.Error
	}
	if t.StartedAt != nil {
		view.StartedAt = t.StartedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if t.FinishedAt != nil {
		view.FinishedAt = t.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	view.Params = decodeJSON(t.Params)
	view.Result = decodeJSON(t.Result)
	return view
}

// decodeJSON 把存储的 JSON 文本解析为对象；无法解析时返回原文，
// 而不是丢弃——原文至少能让排查的人看到「存进去的是什么」。
func decodeJSON(raw *string) any {
	if raw == nil || *raw == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(*raw), &v); err != nil {
		return *raw
	}
	return v
}

func pageParams(c *app.RequestContext) (int, int) {
	page, pageSize := queryInt(c, "page"), queryInt(c, "page_size")
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	return page, pageSize
}

// ClearTasks 清理已完成的旧任务（API-299）。
//
// 两条约束来自服务层，而它们的理由值得在这里也写一遍：
//
//  1. **只清终态任务**——删除一个执行中的任务会让它的结果永远无处落定。
//  2. **被引用的任务不删**——`vm_schedule.last_task_id` 与
//     `network_capture.task_id` 是**当前状态的一部分**（「这条定时任务上次
//     跑的结果」），不是历史。清掉它们指向的任务之后那些引用会悬空，而用户
//     看不出是「任务被清理了」还是「记录坏了」。
//
// 默认保留 7 天：用户点「清理」多半是想清掉旧的，而不是「把刚才那条也删了」。
// 默认全清的话，他刚跑完一个任务、想回头看一眼结果时发现没了。
func (h *Task) ClearTasks(ctx context.Context, c *app.RequestContext) {
	var req struct {
		KeepDays int `json:"keep_days"`
	}
	_ = c.Bind(&req)

	days := req.KeepDays
	if days <= 0 {
		days = 7
	}
	if days > 3650 {
		// **必须给出响应**：静默 return 会让客户端一直等一个永远不来的答复，
		// 而用户看到的是一个转圈不动的界面。上限本身是为了挡住"把 keep_days
		// 填成 999999 以为能清全部"这类误填。
		api.Fail(c, api.InvalidParameter("保留天数不能超过 3650 天"))
		return
	}
	before := time.Now().AddDate(0, 0, -days)

	n, err := h.queue.Cleanup(ctx, before)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{
		"cleared":   n,
		"before":    before.Format(time.RFC3339),
		"keep_days": days,
	})
}
