package model

import "time"

// 告警级别。
const (
	AlertLevelWarning = "warning"
	AlertLevelDanger  = "danger"
)

// 告警状态。
const (
	// AlertActive 正在告警。
	AlertActive = "active"
	// AlertAcked 已被确认（用户看到了，但问题还在）。
	//
	// 确认**不等于解决**：它只是让这条告警不再打扰人。把它做成"关掉"会让
	// 一个仍然存在的问题从视野里消失，而没有任何地方记得它还在。
	AlertAcked = "acked"
	// AlertCleared 问题已消失（自动关闭）。
	AlertCleared = "cleared"
)

// Alert 对应 alert 表：一条**持续存在**的告警。
//
// 做成"持续状态"而不是"事件流"，是因为它要回答的问题不同：
//
//	事件流（audit_log / scheduler_event）回答"发生过什么"；
//	本表回答"现在有什么问题还没处理"。
//
// 用事件流当告警的话，同一个问题每轮评估都会留一条，用户看到的是几百条
// 重复；而用一张状态表，同一问题只有一行，靠 first_at / last_at 表达
// "从什么时候开始、最近一次见到是什么时候"。
type Alert struct {
	ID int64 `gorm:"primaryKey"`
	// NodeID 可为空：个别告警不属于任何节点（如全局的任务失败统计用
	// 不到节点维度时）。多数告警都有节点，便于按节点过滤。
	NodeID *int64

	// Kind 是告警种类（见 internal/alert 包的常量）。
	Kind string `gorm:"size:32;not null"`
	// Level 取值 warning / danger。
	Level string `gorm:"size:16;not null"`

	// ResourceType / ResourceID 定位到具体对象；没有单一对象时用 0。
	ResourceType string `gorm:"size:32;not null;default:''"`
	ResourceID   int64  `gorm:"not null;default:0;uniqueIndex:uniq_alert_kind_resource,priority:2"`
	ResourceName string `gorm:"size:128"`

	Title  string `gorm:"size:255;not null"`
	Detail string `gorm:"type:text"`

	Status string `gorm:"size:16;not null;default:active"`

	// FirstAt 是首次出现时间；LastAt 是最近一次仍成立的时间。
	//
	// 两个都要：只留 LastAt 的话，"这个节点已经离线三天了"与"刚刚掉线"
	// 看起来一样，而处理的紧迫程度完全不同。
	FirstAt time.Time
	LastAt  time.Time

	AckBy     *int64
	AckAt     *time.Time
	ClearedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (Alert) TableName() string { return "alert" }

// IsOpen 报告这条告警是否还"挂在那"——未确认，或确认了但问题仍在。
func (a *Alert) IsOpen() bool { return a.Status == AlertActive || a.Status == AlertAcked }
