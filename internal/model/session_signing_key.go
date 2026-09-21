package model

import "time"

// SessionSigningKey 是会话令牌的签名密钥（当前在用那一条）。
type SessionSigningKey struct {
	ID int64 `gorm:"primaryKey"`
	// KeyID 进令牌的 kid 声明，便于将来定位"这个令牌是用哪把密钥签的"。
	KeyID     string `gorm:"column:key_id;size:32;not null"`
	SecretEnc string `gorm:"column:secret_enc;type:text;not null"`
	Active    bool   `gorm:"not null;default:true"`

	CreatedAt time.Time
	// RotatedAt 是这把密钥生效的时刻（首次创建时即为创建时刻）。
	RotatedAt time.Time
	RotatedBy *int64
}

// TableName 固定表名。
func (SessionSigningKey) TableName() string { return "session_signing_key" }
