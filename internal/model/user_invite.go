package model

import "time"

// UserInvite 对应 user_invite 表：一次邀请。
type UserInvite struct {
	ID    int64  `gorm:"primaryKey"`
	Email string `gorm:"size:128;not null"`
	Role  string `gorm:"size:16;not null;default:tenant"`

	// TokenHash 是邀请令牌的哈希。
	//
	// 令牌会出现在邮件与聊天记录里（都是不可控的地方），而库里的哈希即使
	// 泄漏也换不回那个链接。
	// 唯一索引与迁移 uniq_user_invite_token 对齐：库里只存哈希、令牌只在
	// 签发响应里出现一次，重发即换新——这条约束是"旧链接必然作废"的
	// 数据库层兜底（index_alignment_test 抓到过模型侧漏声明）。
	TokenHash string `gorm:"column:token_hash;size:128;not null;uniqueIndex:uniq_user_invite_token"`

	QuotaBytes   int64   `gorm:"column:quota_bytes;not null;default:0"`
	QuotaEnabled bool    `gorm:"column:quota_enabled;not null;default:false"`
	Remark       *string `gorm:"size:255"`

	ExpiresAt time.Time
	// AcceptedAt 非空即表示已使用；邀请**一次性**，用完不能再用来注册第二个
	// 账号。
	AcceptedAt     *time.Time
	AcceptedUserID *int64
	// RevokedAt 非空表示被管理员撤销。
	RevokedAt *time.Time

	CreatedBy *int64
	CreatedAt time.Time
}

// TableName 固定表名。
func (UserInvite) TableName() string { return "user_invite" }

// Usable 报告这条邀请此刻是否可用。
//
// 过期与已撤销返回 false 但**不区分原因**：对外一律说"邀请无效"，否则一个
// 猜令牌的人能据此判断自己猜的是一条被用过的还是过期的。
func (u *UserInvite) Usable(now time.Time) bool {
	return u.AcceptedAt == nil && u.RevokedAt == nil && u.ExpiresAt.After(now)
}
