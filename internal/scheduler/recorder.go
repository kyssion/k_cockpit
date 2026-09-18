package scheduler

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// Recorder 写调度事件。
//
// 它**只应该在有实际动作时被调用**（F-7-04：只记录实际发生的调度动作）。
// 调用点写成「if 做了事 { recorder.Record(...) }」而不是「每轮都记一条」——
// 后者会让一个 30 秒周期的调度器一天产生 2880 条噪声，把真正有价值的
// 那几条埋掉。
type Recorder struct {
	db       *gorm.DB
	registry *Registry
}

// NewRecorder 构造记录器。
func NewRecorder(db *gorm.DB, registry *Registry) *Recorder {
	return &Recorder{db: db, registry: registry}
}

// Event 是一次待记录的动作。
type Event struct {
	// Key 是调度器标识，必须在注册表里。
	Key string
	// NodeID 是动作发生的节点，0 表示不针对具体节点。
	NodeID int64
	// Scope 是这次动作影响的范围（节点名 / 虚拟机名 / 一句概括）。
	Scope string
	// Status 取 model.SchedulerDone 或 model.SchedulerFailed。
	Status string
	// Message 写清**做了什么**，而不只是「成功」。
	//
	// 「成功」这个词在事件列表里等于没说——用户看不出这次调度到底动了
	// 什么。要写「清理了 500 条已完成任务」这样能回答「那我需要做什么吗」
	// 的话。
	Message string
}

// Record 写入一条事件。
//
// **失败只记日志，不向上返回错误。** 记事件是观测手段，被观测的动作已经
// 发生完了；让一次日志写入的失败去影响调度本身，等于让观测改变被观测的
// 东西——而那种影响在排查时会以最意外的方式出现（"为什么昨晚的清理没跑"
// 的答案是"因为事件表写满了"）。
func (r *Recorder) Record(ctx context.Context, e Event) {
	if r == nil || r.db == nil || e.Key == "" {
		return
	}
	name, group := e.Key, ""
	if r.registry != nil {
		if info, ok := r.registry.Get(e.Key); ok {
			name, group = info.Name, info.Group
		}
	}
	if !model.ValidSchedulerStatus(e.Status) {
		e.Status = model.SchedulerDone
	}

	row := model.SchedulerEvent{
		SchedulerKey: e.Key,
		Status:       e.Status,
		At:           time.Now(),
	}
	row.SchedulerName = &name
	if group != "" {
		row.GroupName = &group
	}
	if e.NodeID > 0 {
		row.NodeID = &e.NodeID
	}
	if e.Scope != "" {
		row.Scope = &e.Scope
	}
	if e.Message != "" {
		row.Message = &e.Message
	}

	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[scheduler] 写入调度事件失败 key=%s: %v", e.Key, err)
	}
}

// Service 提供调度器与事件的查询（F-7-04）。
type Service struct {
	db       *gorm.DB
	registry *Registry
}

// NewService 构造查询服务。
func NewService(db *gorm.DB, registry *Registry) *Service {
	return &Service{db: db, registry: registry}
}

// SchedulerView 是调度器 + 它最近的事件。
type SchedulerView struct {
	Info Info `json:"scheduler"`
	// Events 是最近几次**实际发生的动作**，可能为空。
	Events []EventView `json:"events"`
	// LastActionAt 最近一次动作的时刻；为空表示**还没有过动作**。
	//
	// 与「没有运行」是两回事：一个每 30 秒醒一次、但今天确实无事可做的
	// 清理器，LastActionAt 会是昨天。界面必须把这两个概念分开说。
	LastActionAt string `json:"last_action_at,omitempty"`
	FailedCount  int    `json:"failed_count"`
}

// EventView 是对外的事件视图。
type EventView struct {
	ID      int64  `json:"id"`
	Key     string `json:"scheduler_key"`
	Name    string `json:"scheduler_name,omitempty"`
	Group   string `json:"group,omitempty"`
	NodeID  int64  `json:"node_id,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	At      string `json:"at"`
}

// Overview 返回全部调度器及其最近事件。
//
// 每个调度器都返回**即使它没有任何事件**：注册表是"有哪些东西在跑"的答案，
// 而事件表只在该做事的时候才有行。只返回有事件的那些，会让一个正常但最近
// 没事可做的调度器凭空消失。
func (s *Service) Overview(ctx context.Context, perScheduler int) ([]SchedulerView, error) {
	if perScheduler <= 0 {
		perScheduler = 5
	}
	groups := s.registry.Grouped()

	out := make([]SchedulerView, 0, 16)
	for _, g := range groups {
		for _, info := range g.Schedulers {
			view := SchedulerView{Info: info, Events: []EventView{}}

			var rows []model.SchedulerEvent
			if err := s.db.WithContext(ctx).
				Where("scheduler_key = ?", info.Key).
				Order("at DESC, id DESC").
				Limit(perScheduler).Find(&rows).Error; err != nil {
				return nil, err
			}
			for i := range rows {
				view.Events = append(view.Events, toEventView(&rows[i]))
				if rows[i].Status == model.SchedulerFailed {
					view.FailedCount++
				}
			}
			if len(view.Events) > 0 {
				view.LastActionAt = view.Events[0].At
			}
			out = append(out, view)
		}
	}
	return out, nil
}

// Events 返回事件流，可按调度器与状态筛选。
func (s *Service) Events(ctx context.Context, key, status string, limit int) ([]EventView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Model(&model.SchedulerEvent{})
	if key != "" {
		q = q.Where("scheduler_key = ?", key)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}

	var rows []model.SchedulerEvent
	if err := q.Order("at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]EventView, 0, len(rows))
	for i := range rows {
		out = append(out, toEventView(&rows[i]))
	}
	return out, nil
}

func toEventView(e *model.SchedulerEvent) EventView {
	v := EventView{
		ID: e.ID, Key: e.SchedulerKey, Status: e.Status,
		At: e.At.Format("2006-01-02T15:04:05Z07:00"),
	}
	if e.SchedulerName != nil {
		v.Name = *e.SchedulerName
	}
	if e.GroupName != nil {
		v.Group = *e.GroupName
	}
	if e.NodeID != nil {
		v.NodeID = *e.NodeID
	}
	if e.Scope != nil {
		v.Scope = *e.Scope
	}
	if e.Message != nil {
		v.Message = *e.Message
	}
	return v
}
