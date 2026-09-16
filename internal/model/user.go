// Package model 定义数据库实体与表结构的映射。
//
// 字段以 docs/02-architecture/DATA_MODEL.md 为准，表结构由
// internal/database/migrations/ 下的迁移管理，模型不做结构变更。
//
// 模型只声明当前实现用到的字段：GORM 生成 SQL 时按已声明字段列表操作，
// 未声明的列不会被读写，因此可以随能力交付逐步补齐。
package model

import (
	"time"

	"gorm.io/gorm"
)

// 用户角色。固定枚举，不支持自定义角色（f-1-06 R-001）。
const (
	RoleAdmin  = "admin"
	RoleTenant = "tenant"
)

// 用户状态。
const (
	UserStatusPending = "pending" // 待激活
	UserStatusActive  = "active"  // 正常
	UserStatusBanned  = "banned"  // 已封禁
)

// User 对应 user 表。
type User struct {
	ID int64 `gorm:"primaryKey"`
	// 索引与迁移一致（uniq_user_username）。用户名唯一是登录的前提：
	// 没有它，同名用户可以存在两个，「用用户名找用户」就会拿到不确定的一个。
	Username            string  `gorm:"size:64;not null;uniqueIndex:uniq_user_username"`
	PasswordHash        string  `gorm:"size:255;not null"`
	Role                string  `gorm:"size:16;not null;default:tenant"`
	Status              string  `gorm:"size:16;not null;default:pending"`
	Email               *string `gorm:"size:128"`
	EmailVerifiedAt     *time.Time
	TotpSecretEnc       *string `gorm:"type:text"`
	TotpEnabled         bool    `gorm:"not null;default:false"`
	RecoveryCodesHash   *string `gorm:"type:text"`
	BootstrapSkipped    bool    `gorm:"not null;default:false"`
	ForcePasswordChange bool    `gorm:"not null;default:false"`
	// SecurityUpdatedAt 记录安全信息变更时间（改密 / 改用户名 / 2FA 变更 /
	// 恢复码重置）。更新它会让该用户**全部既有会话立即失效**（f-1-01 R-010）。
	SecurityUpdatedAt *time.Time
	Remark            *string `gorm:"size:255"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// 用 gorm.DeletedAt 而非 *time.Time：前者让 GORM **自动**为所有查询
	// 追加 `deleted_at IS NULL`，避免某处漏写条件导致已删除的账号仍可登录。
	DeletedAt gorm.DeletedAt
}

// TableName 固定表名：PostgreSQL 中 user 是保留字，迁移里写作 "user"，
// 且 GORM 默认会复数化为 users，必须显式指定。
func (User) TableName() string { return "user" }

// IsAdmin 报告用户是否为管理员。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// IsActive 报告账号是否可登录（未被封禁、未软删除、已激活）。
//
// 注意：调用方不应据此区分对外文案——登录失败统一提示（f-1-01 R-002），
// 该判断只用于决定内部流程。
func (u *User) IsActive() bool {
	return u.Status == UserStatusActive && !u.DeletedAt.Valid
}
