// Package passaudit 周期检查账号口令是否出现在弱口令 / 已知泄露清单中（F-10-06）。
//
// 判定在**节点侧**完成：真正的泄露比对需要一份大清单（或以 k-anonymity 方式
// 查询外部服务），而控制面既不联网、也不持有明文密码——它只有 argon2 哈希，
// 无从"比对"。因此这里只负责开关、定时与结果落地，判定交给节点。
//
// 命中**不自动改密**：那是用户自己的凭据，系统替他改掉会让他自己也不知道
// 新密码是什么。这里只把命中标出来，由他在安全中心改。
package passaudit

import (
	"context"
	"log"
	"time"

	"encoding/json"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// Service 提供口令检查。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	// enabled 读取设置项 security.password_breach_check。
	enabled func() bool
	audit   *audit.Recorder
}

// NewService 构造服务。
func NewService(db *gorm.DB, client agent.Client, enabled func() bool, recorder *audit.Recorder) *Service {
	return &Service{db: db, agent: client, enabled: enabled, audit: recorder}
}

// Hit 是一个命中项。
type Hit struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Reason   string `json:"reason,omitempty"`
}

// Result 是一次检查的结果。
type Result struct {
	Checked int    `json:"checked"`
	Hits    []Hit  `json:"hits"`
	Message string `json:"message,omitempty"`
	// Unavailable 非空表示节点没实现，因此**没有真正检查**。
	Unavailable string `json:"unavailable,omitempty"`
}

// RunNow 立即检查一次。
func (s *Service) RunNow(ctx context.Context) (*Result, error) {
	if s.enabled != nil && !s.enabled() {
		return nil, api.ValidationFailed("定时口令检查未开启，如需立即检查请先打开开关")
	}
	if s.agent == nil {
		return &Result{Unavailable: "未连接节点，无法检查"}, nil
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpSecurityPasswordAudit,
		NodeID: 0,
		Target: "passwords",
	})
	if err != nil {
		return &Result{Unavailable: "节点不可达，未检查"}, nil
	}
	if !result.Success {
		return &Result{Unavailable: result.Message}, nil
	}
	info := decodeAudit(result.Data)

	// 先把所有人的命中状态清掉，再按节点结果重新标。
	//
	// 顺序不能反：先标后清会把刚标上的命中一起清掉，表现为"明明检查出问题
	// 了，界面上却什么都没标"。
	now := time.Now()
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("breach_hit = ?", true).
		Updates(map[string]any{"breach_hit": false, "breach_checked_at": now}).Error; err != nil {
		log.Printf("[passaudit] 清理命中状态失败: %v", err)
	}

	out := &Result{Checked: info.Checked, Message: info.Message}
	for _, h := range info.Hits {
		var user model.User
		if err := s.db.WithContext(ctx).Where("username = ?", h.Username).First(&user).Error; err != nil {
			// 节点返回了一个不存在的用户名：跳过而不是整体失败——一条对不上
			// 的记录不该让整次检查的结果都不可用。
			log.Printf("[passaudit] 未找到用户 %q: %v", h.Username, err)
			continue
		}
		if err := s.db.WithContext(ctx).Model(&model.User{}).
			Where("id = ?", user.ID).
			Updates(map[string]any{"breach_hit": true, "breach_checked_at": now}).Error; err != nil {
			log.Printf("[passaudit] 标记命中失败 user=%d: %v", user.ID, err)
			continue
		}
		out.Hits = append(out.Hits, Hit{UserID: user.ID, Username: user.Username, Reason: h.Reason})
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			ResourceType: "user", Action: "user.password_audit",
			Params:  map[string]any{"checked": info.Checked, "hits": len(out.Hits)},
			Success: true,
		})
	}
	return out, nil
}

// Status 汇总当前有多少账号处于命中状态。
func (s *Service) Status(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("breach_hit = ?", true).Count(&n).Error; err != nil {
		log.Printf("[passaudit] 统计命中失败: %v", err)
		return 0, api.Internal()
	}
	return n, nil
}

func (s *Service) usernames(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	var users []model.User
	if err := s.db.WithContext(ctx).Select("id", "username").Find(&users).Error; err != nil {
		log.Printf("[passaudit] 查询用户失败: %v", err)
		return out
	}
	for i := range users {
		out[users[i].Username] = users[i].ID
	}
	return out
}

func decodeAudit(data map[string]any) agent.PasswordAuditInfo {
	if data == nil {
		return agent.PasswordAuditInfo{}
	}
	raw, ok := data[agent.PasswordAuditDataKey]
	if !ok {
		return agent.PasswordAuditInfo{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.PasswordAuditInfo{}
	}
	var info agent.PasswordAuditInfo
	if err := json.Unmarshal(blob, &info); err != nil {
		log.Printf("[passaudit] 解析检查结果失败: %v", err)
		return agent.PasswordAuditInfo{}
	}
	return info
}
