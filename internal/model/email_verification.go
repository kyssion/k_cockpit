package model

import "time"

// 验证码用途。
const (
	// EmailCodeBind 用于绑定（或改绑）邮箱。
	EmailCodeBind = "bind"
	// EmailCodeReset 用于找回密码。
	//
	// 与 bind 分开的原因不只是语义：找回密码的码等价于一次改密许可，
	// 有效期更短、且校验通过后还要再走一次重置接口。混用一个 kind 会让
	// 「绑定邮箱的码被拿去改密码」成为可能。
	EmailCodeReset = "reset"
)

// EmailVerification 对应 email_verification 表：一次发出的邮箱验证码。
type EmailVerification struct {
	ID int64 `gorm:"primaryKey"`
	// UserID 为空是正常的：找回密码时用户是通过邮箱反查出来的，而发码
	// 那一刻**不能**确认该邮箱是否存在——确认了就等于给了枚举账号的接口。
	UserID *int64

	Email string `gorm:"size:128;not null"`
	Kind  string `gorm:"size:16;not null"`
	// CodeHash 是验证码的哈希（bcrypt 成本较低的一档：它是 6 位数字，
	// 熵本来就小，提高成本只会拖慢校验而挡不住爆破——挡爆破靠 attempts）。
	CodeHash string `gorm:"size:255;not null"`

	// Attempts 是已尝试次数。上限由服务层判定，这里只记数。
	Attempts   int       `gorm:"not null;default:0"`
	ExpiresAt  time.Time `gorm:"not null"`
	ConsumedAt *time.Time
	// TicketUsedAt 记下由这个验证码换发的重置票据被使用的时刻。
	//
	// 票据本身是无状态 JWT，签名有效就能重复提交；没有这一列，"改密码"
	// 这个动作在票据有效期内可被重放任意次——用户刚改完密码，拿着同一个
	// 链接的人还能再改一次。
	TicketUsedAt *time.Time
	ClientIP     *string `gorm:"size:64"`

	CreatedAt time.Time
}

// TableName 固定表名。
func (EmailVerification) TableName() string { return "email_verification" }

// Expired 报告验证码是否已过期。
func (e *EmailVerification) Expired(now time.Time) bool { return !e.ExpiresAt.After(now) }

// Usable 报告验证码是否仍可用：未过期、未被使用、未超出尝试上限。
func (e *EmailVerification) Usable(now time.Time, maxAttempts int) bool {
	return !e.Expired(now) && e.ConsumedAt == nil && e.Attempts < maxAttempts
}
