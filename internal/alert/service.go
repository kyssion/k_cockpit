// Package alert 实现告警中心（F-8-07）。
//
// 工作台的状态横幅回答的是"这一刻有没有事"，本包回答的是"有哪些事还没
// 处理、它们从什么时候开始、谁确认过"。前者是首页的一瞥，后者是要能
// 翻、能确认、能事后复盘的一份清单。
package alert

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// 告警种类。取值同时是去重的键，因此**不可随意改名**——改名会让历史告警
// 变成另一条新告警。
const (
	KindNodeOffline     = "node_offline"
	KindNodePending     = "node_pending"
	KindNodeMaintenance = "node_maintenance"
	KindVMMissing       = "vm_missing"
	KindTaskFailed      = "task_failed"
	KindQuotaLimited    = "quota_limited"
	KindStorageLow      = "storage_low"
	KindHostCPU         = "host_cpu_high"
	KindHostMemory      = "host_memory_high"
)

// 阈值。
//
// 全部是**常量而不是配置项**：它们描述的是"什么值得打断人"，属于产品判断；
// 做成可调项之后，每个部署的告警含义都不一样，而用户跨环境看面板时会
// 以为同一块黄色在这里和那里是同一件事。
const (
	cpuHighPercent   = 90
	memHighPercent   = 90
	storageLowGB     = 10
	failedTaskWindow = 24 * time.Hour
)

// Service 提供告警的评估与查询。
type Service struct {
	db *gorm.DB
}

// NewService 构造服务。
func NewService(db *gorm.DB) *Service { return &Service{db: db} }

// Item 是一条告警的视图。
type Item struct {
	ID       int64   `json:"id"`
	NodeID   *int64  `json:"node_id,omitempty"`
	Kind     string  `json:"kind"`
	Level    string  `json:"level"`
	Title    string  `json:"title"`
	Detail   string  `json:"detail,omitempty"`
	Status   string  `json:"status"`
	Resource string  `json:"resource,omitempty"`
	FirstAt  string  `json:"first_at"`
	LastAt   string  `json:"last_at"`
	AckAt    *string `json:"ack_at,omitempty"`
}

// ListOptions 是查询条件。
type ListOptions struct {
	Status string
	Level  string
	Limit  int
}

// List 列出告警，未确认的在前、严重的在前。
func (s *Service) List(ctx context.Context, opts ListOptions) ([]Item, error) {
	query := s.db.WithContext(ctx).Model(&model.Alert{}).
		Where("status != ? OR status IS NULL", model.AlertCleared)
	if opts.Status != "" {
		query = query.Where("status = ?", opts.Status)
	}
	if opts.Level != "" {
		query = query.Where("level = ?", opts.Level)
	}
	limit := opts.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	var rows []model.Alert
	if err := query.Order("status = 'active' DESC, level = 'danger' DESC, last_at DESC").
		Limit(limit).Find(&rows).Error; err != nil {
		log.Printf("[alert] 查询告警失败: %v", err)
		return nil, api.Internal()
	}
	out := make([]Item, 0, len(rows))
	for i := range rows {
		out = append(out, toItem(&rows[i]))
	}
	return out, nil
}

// Counts 返回未确认与严重的数量，供界面显示徽标。
func (s *Service) Counts(ctx context.Context) (active int, danger int, err error) {
	var a, d int64
	if err := s.db.WithContext(ctx).Model(&model.Alert{}).
		Where("status = ?", model.AlertActive).Count(&a).Error; err != nil {
		log.Printf("[alert] 统计未确认告警失败: %v", err)
		return 0, 0, api.Internal()
	}
	if err := s.db.WithContext(ctx).Model(&model.Alert{}).
		Where("status = ? AND level = ?", model.AlertActive, model.AlertLevelDanger).
		Count(&d).Error; err != nil {
		log.Printf("[alert] 统计严重告警失败: %v", err)
		return 0, 0, api.Internal()
	}
	return int(a), int(d), nil
}

// Ack 确认一条告警。
func (s *Service) Ack(ctx context.Context, id int64, v authz.Viewer) error {
	now := time.Now()
	res := s.db.WithContext(ctx).Model(&model.Alert{}).
		Where("id = ? AND status != ?", id, model.AlertCleared).
		Updates(map[string]any{
			"status": model.AlertAcked,
			"ack_by": v.UserID,
			"ack_at": now,
		})
	if res.Error != nil {
		log.Printf("[alert] 确认告警失败 id=%d: %v", id, res.Error)
		return api.Internal()
	}
	if res.RowsAffected == 0 {
		return api.NotFound("告警不存在或已关闭")
	}
	return nil
}

// AckAll 确认当前全部未确认告警。
func (s *Service) AckAll(ctx context.Context, v authz.Viewer) (int, error) {
	now := time.Now()
	res := s.db.WithContext(ctx).Model(&model.Alert{}).
		Where("status = ?", model.AlertActive).
		Updates(map[string]any{
			"status": model.AlertAcked,
			"ack_by": v.UserID,
			"ack_at": now,
		})
	if res.Error != nil {
		log.Printf("[alert] 批量确认告警失败: %v", res.Error)
		return 0, api.Internal()
	}
	return int(res.RowsAffected), nil
}

func toItem(a *model.Alert) Item {
	resource := a.ResourceName
	if resource == "" && a.ResourceID > 0 {
		resource = a.ResourceType + " #"
	}
	item := Item{
		ID: a.ID, NodeID: a.NodeID, Kind: a.Kind, Level: a.Level,
		Title: a.Title, Detail: a.Detail, Status: a.Status, Resource: resource,
		FirstAt: a.FirstAt.UTC().Format(time.RFC3339),
		LastAt:  a.LastAt.UTC().Format(time.RFC3339),
	}
	if a.AckAt != nil {
		t := a.AckAt.UTC().Format(time.RFC3339)
		item.AckAt = &t
	}
	return item
}
