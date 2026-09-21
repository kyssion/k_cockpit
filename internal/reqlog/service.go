// Package reqlog 记录与查询接口调用日志（F-10-07）。
//
// 它与审计的分工：审计回答"谁改了什么"，请求日志回答"接口被调用了多少次、
// 慢不慢、返回了什么状态码"。两者量级差两个数量级，因此分表，且请求日志
// **默认关闭**——默认打开只会把磁盘用在心跳与轮询上。
package reqlog

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// Service 提供请求日志的写入与查询。
type Service struct {
	db *gorm.DB
	// enabled 决定是否落库。它由设置项驱动（security.request_log_enabled），
	// 每次写入前读取——改了设置立即生效，而不是等重启。
	enabled func(context.Context) bool
}

// NewService 构造服务。enabled 返回当前是否记录；为 nil 时一律不记录。
func NewService(db *gorm.DB, enabled func(context.Context) bool) *Service {
	return &Service{db: db, enabled: enabled}
}

// Enabled 报告当前是否记录请求日志。
func (s *Service) Enabled(ctx context.Context) bool {
	if s.enabled == nil {
		return false
	}
	return s.enabled(ctx)
}

// Entry 是一次要记录的调用。
type Entry struct {
	UserID     *int64
	Method     string
	Path       string
	Status     int
	DurationMS int
	ClientIP   *string
	UserAgent  *string
}

// Record 写入一条请求日志。
//
// 失败只记日志而**不返回错误**：日志是旁路，为"记不下来"去影响一次正常的
// 接口响应是最不划算的取舍。
func (s *Service) Record(ctx context.Context, e Entry) {
	if !s.Enabled(ctx) {
		return
	}
	row := model.RequestLog{
		At:         time.Now(),
		UserID:     e.UserID,
		Method:     e.Method,
		Path:       e.Path,
		Status:     e.Status,
		DurationMS: e.DurationMS,
		ClientIP:   e.ClientIP,
		UserAgent:  e.UserAgent,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[reqlog] 写入请求日志失败: %v", err)
	}
}

// Item 是列表里的一条。
type Item struct {
	ID         int64  `json:"id"`
	At         string `json:"at"`
	UserID     *int64 `json:"user_id,omitempty"`
	Username   string `json:"username,omitempty"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Status     int    `json:"status"`
	DurationMS int    `json:"duration_ms"`
	ClientIP   string `json:"client_ip,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
}

// List 查询最近的请求日志。
func (s *Service) List(ctx context.Context, userID int64, limit int) ([]Item, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Model(&model.RequestLog{}).Order("at DESC").Limit(limit)
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}

	var rows []model.RequestLog
	if err := q.Find(&rows).Error; err != nil {
		log.Printf("[reqlog] 查询请求日志失败: %v", err)
		return nil, api.Internal()
	}

	names := s.usernames(ctx, rows)
	out := make([]Item, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		item := Item{
			ID: r.ID, At: r.At.Format("2006-01-02T15:04:05Z07:00"),
			UserID: r.UserID, Method: r.Method, Path: r.Path,
			Status:     r.Status,
			DurationMS: r.DurationMS,
		}
		if r.ClientIP != nil {
			item.ClientIP = *r.ClientIP
		}
		if r.UserAgent != nil {
			item.UserAgent = *r.UserAgent
		}
		if r.UserID != nil {
			if n, ok := names[*r.UserID]; ok {
				item.Username = n
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// Clear 清理请求日志。
//
// keepDays > 0 时只删这个天数之前的，否则清空全部。留一个"只删旧的"是因为
// 常见需求是"太占地方了"，而不是"现在这些数据也没用了"。
func (s *Service) Clear(ctx context.Context, keepDays int) (int64, error) {
	q := s.db.WithContext(ctx).Model(&model.RequestLog{})
	if keepDays > 0 {
		q = q.Where("at < ?", time.Now().AddDate(0, 0, -keepDays))
	} else {
		// 清空全部用永真条件：GORM 不允许无条件批量删除（安全默认值）。
		q = q.Where("at < ?", time.Now())
	}
	res := q.Delete(&model.RequestLog{})
	if res.Error != nil {
		log.Printf("[reqlog] 清理请求日志失败: %v", res.Error)
		return 0, api.Internal()
	}
	return res.RowsAffected, nil
}

func (s *Service) usernames(ctx context.Context, rows []model.RequestLog) map[int64]string {
	out := map[int64]string{}
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		if rows[i].UserID != nil {
			ids = append(ids, *rows[i].UserID)
		}
	}
	if len(ids) == 0 {
		return out
	}
	var users []model.User
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).
		Select("id", "username").Find(&users).Error; err != nil {
		log.Printf("[reqlog] 查询用户名失败: %v", err)
		return out
	}
	for i := range users {
		out[users[i].ID] = users[i].Username
	}
	return out
}
