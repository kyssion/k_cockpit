package model

import "time"

// 令牌分级（f-1-01 R-006）。不同级别的令牌只能访问对应的接口集合。
const (
	TokenTypeAccess    = "access"    // 可访问全部业务接口
	TokenTypeLogin     = "login"     // 仅可访问登录后续阶段接口（2FA 等）
	TokenTypeBootstrap = "bootstrap" // 仅可访问安全初始化接口
)

// Session 对应 user_session 表。
//
// 会话是服务端可撤销的凭据：令牌签名有效但会话已撤销时，请求仍被拒绝
// （f-1-01 R-005）。
type Session struct {
	ID int64 `gorm:"primaryKey"`
	// 索引与迁移一致（uniq_user_session_session_id）。它保证同一个会话
	// 标识不会落成两行——那会让「撤销会话」只撤销掉其中一行。
	SessionID   string    `gorm:"size:64;not null;uniqueIndex:uniq_user_session_session_id"`
	UserID      int64     `gorm:"not null"`
	TokenType   string    `gorm:"size:24;not null;default:access"`
	Fingerprint *string   `gorm:"size:128"`
	ClientIP    *string   `gorm:"size:64"`
	UserAgent   *string   `gorm:"size:255"`
	IssuedAt    time.Time `gorm:"not null"`
	ExpiresAt   time.Time `gorm:"not null"`
	// LastActiveAt 只由**真实用户活动**更新；轮询与 SSE 心跳不更新（f-1-01 R-008）。
	LastActiveAt *time.Time
	RevokedAt    *time.Time
}

// TableName 固定表名。
func (Session) TableName() string { return "user_session" }

// IsRevoked 报告会话是否已被撤销（登出 / 安全信息变更 / 封禁）。
func (s *Session) IsRevoked() bool { return s.RevokedAt != nil }

// IsExpired 报告会话是否已过期。
func (s *Session) IsExpired(now time.Time) bool { return !now.Before(s.ExpiresAt) }
