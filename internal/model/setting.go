package model

import "time"

// SystemSetting 对应 system_setting 表。
//
// 它是**键值存储**而不是逐项建列（f-9-01 Q-003）：设置项会随版本持续增加
// （规格本身就按 7 个标签分期交付），逐项建列意味着每次加一项都要迁移。
//
// 这里只存**用户显式改过的值**；默认值与元数据在代码中声明（Q-004）——
// 元数据是开发者契约，随版本走，放进库会形成「代码与数据两个来源」，
// 而它们迟早会不一致。
type SystemSetting struct {
	Key   string  `gorm:"primaryKey;size:128"`
	Value *string `gorm:"type:text"`
	// PreviousValue 是最近一次变更前的值，供回滚使用（R-008）。
	//
	// 只保留一次：回滚是低频操作，更早的历史由审计流水承担。
	PreviousValue *string `gorm:"type:text"`
	UpdatedBy     *int64
	UpdatedAt     time.Time
	CreatedAt     time.Time
}

// TableName 固定表名。
func (SystemSetting) TableName() string { return "system_setting" }
