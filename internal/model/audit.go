package model

import "time"

// 审计事件来源。
const (
	SourceWeb       = "web"
	SourceAPI       = "api"
	SourceScheduler = "scheduler"
	SourceSystem    = "system"
)

// AuditLog 对应 audit_log 表。
//
// 这是**只增不改**的流水表：任何写操作与安全事件都应记录（DATA_MODEL §4.14）。
// 操作人名称与资源名以「快照」形式冗余存储——用户改名或资源删除后，
// 历史审计仍须能还原当时的事实。
type AuditLog struct {
	ID           int64     `gorm:"primaryKey"`
	At           time.Time `gorm:"not null"`
	OperatorID   *int64
	OperatorName *string `gorm:"size:64"`
	Source       string  `gorm:"size:16;not null;default:web"`
	NodeID       *int64
	ResourceType string `gorm:"size:32;not null"`
	ResourceID   *int64
	ResourceName *string `gorm:"size:128"`
	Action       string  `gorm:"size:48;not null"`
	Params       *string `gorm:"type:text"`
	BeforeState  *string `gorm:"type:text"`
	AfterState   *string `gorm:"type:text"`
	// Success 刻意**不带 default tag**：GORM 对带 default 的字段会跳过零值，
	// 导致失败记录（false）被数据库默认值 true 覆盖——审计会因此把失败写成成功。
	// 表结构本身仍保留默认值，模型这里只约束写入行为。
	Success  bool    `gorm:"not null"`
	Error    *string `gorm:"size:512"`
	ClientIP *string `gorm:"size:64"`
}

// TableName 固定表名。
func (AuditLog) TableName() string { return "audit_log" }
