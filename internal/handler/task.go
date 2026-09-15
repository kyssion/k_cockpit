package handler

import (
	"context"
	"encoding/json"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Task 提供任务接口（F-7-02）。
type Task struct {
	queue *task.Queue
}

// NewTask 构造任务接口。
func NewTask(queue *task.Queue) *Task {
	return &Task{queue: queue}
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
	api.OK(c, toTaskView(t))
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
