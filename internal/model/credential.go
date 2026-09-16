package model

import "time"

// CredentialVNC 是控制台密码在 vm_credential 表中的 username 取值。
//
// vm_credential 一行对应一台虚拟机的凭据，用 username 区分用途：控制台
// 密码没有用户名，但复用这张表比新建一张只有一列的表更合适——它们的
// 加密方式、生命周期与归属完全一致。
const CredentialVNC = "_vnc"

// VMCredential 对应 vm_credential 表。
//
// PasswordEnc 保存的是**密文**（列名即以 _enc 结尾）。这里的每一个值都
// 需要能被还原成明文交给 agent 或参与协议认证，因此不能用单向哈希——
// 但也因此，它是本项目里最需要保护的数据之一。
type VMCredential struct {
	ID       int64   `gorm:"primaryKey"`
	VMID     int64   `gorm:"not null"`
	Username *string `gorm:"size:64"`
	// PasswordEnc 是 AES-GCM 密文（见 internal/cryptoutil）。
	PasswordEnc string `gorm:"type:text;not null"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMCredential) TableName() string { return "vm_credential" }
