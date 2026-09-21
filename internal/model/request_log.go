package model

import "time"

// RequestLog 对应 request_log 表：一次接口调用。
//
// 它**没有**对 User 的外键：用户被删除后这些日志仍然应该有意义（"那台机器
// 是谁在什么时候调的"），物理外键会让删用户变成一件要连带删日志的事。
type RequestLog struct {
	ID     int64 `gorm:"primaryKey"`
	At     time.Time
	UserID *int64

	Method string `gorm:"size:8;not null"`
	Path   string `gorm:"size:512;not null"`
	// Status 是 HTTP 状态码。它与业务成败是两回事——一个 200 也可能是一次
	// 失败的业务操作，因此排查时要同时看审计。
	Status     int `gorm:"not null"`
	DurationMS int `gorm:"column:duration_ms;not null;default:0"`

	ClientIP  *string `gorm:"size:64"`
	UserAgent *string `gorm:"size:255"`
}

// TableName 固定表名。
func (RequestLog) TableName() string { return "request_log" }
