// Package model 定义 GORM 数据模型。
//
// 模型命名使用单数形式，配合 database 包中的 SingularTable 策略，
// 表名即为模型名本身（User -> user）。
package model

import "time"

// User 是示例模型，用于演示 GORM 的基本用法与自动迁移。
//
// 真实业务模型应按模块拆分文件；示例模型可在正式开发时删除。
type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:64;not null;index" json:"name"`
	Email     string    `gorm:"size:128;not null;uniqueIndex" json:"email"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
