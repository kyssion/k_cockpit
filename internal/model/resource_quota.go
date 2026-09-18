package model

import "time"

// 配额维度。
//
// 全部是**累计型**（一段时间内累计多少），因此都有「超限」这个概念。
//
// 带宽限速是**速率型**——它没有累计，也就没有"超限"这回事（限速一旦生效
// 就一直在生效，不存在"用满了才限"）。把它混进这张表会让人以为带宽也有
// 一个用满的状态，因此它不走这里。
const (
	QuotaDimTrafficIn  = "traffic_in"
	QuotaDimTrafficOut = "traffic_out"
	QuotaDimRuntime    = "runtime"
)

// 超限后的处置。
const (
	// QuotaActionThrottle 限速：还能用，但变慢。
	QuotaActionThrottle = "throttle"
	// QuotaActionBlock 断网：停掉。
	//
	// **不是默认值**：它会让业务直接中断。选它的人应当是有意为之，
	// 而不是"没注意"。
	QuotaActionBlock = "block"
)

// 配额状态。
const (
	// QuotaStatusOK 未超限（含接近但未达阈值）。
	QuotaStatusOK = "ok"
	// QuotaStatusWarned 已超过预警阈值但未超上限。
	QuotaStatusWarned = "warned"
	// QuotaStatusLimited 已超限并**已处置**。
	//
	// 与 warned 分开是刻意的：前者是"快到了"，后者是"已经处置了"。
	// 合并成一个「超限」状态的话，用户在网络变慢时无法判断是自己用超了
	// 还是会话出了问题。
	QuotaStatusLimited = "limited"
)

// WarnRatioPercent 是预警阈值（占上限的百分比）。
//
// 80%：留出五分之一的空间，用户看到预警后还来得及做点什么（清数据、
// 申请提额）。设得更高（比如 95%）会让预警与超限几乎同时发生，那预警
// 就失去了它唯一的用途。
const WarnRatioPercent = 80

// ResourceQuota 对应 resource_quota 表：用户 × 维度的上限与处置状态（F-4-10）。
//
// 它**只放策略与处置状态，不放用量**。用量有两张按天累计的表在采
// （traffic_stat_daily / vm_runtime_daily），在这里再存一份会出现两个
// 数据源——而两者对不上时无法判断哪个是真的，那种分歧不会报错，只会让
// 用户看到的数字与处置依据不一致。
type ResourceQuota struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"column:node_id;not null;uniqueIndex:uniq_resource_quota_node_user_dim,priority:1"`
	UserID int64 `gorm:"column:user_id;not null;uniqueIndex:uniq_resource_quota_node_user_dim,priority:2"`

	Dimension string `gorm:"size:24;not null;uniqueIndex:uniq_resource_quota_node_user_dim,priority:3"`

	// LimitValue 是上限。**0 表示不限**。
	LimitValue int64 `gorm:"column:limit_value;not null;default:0"`

	// Action 是超限后的处置。
	Action string `gorm:"size:16;not null;default:throttle"`

	// Status 是当前周期的状态。
	Status string `gorm:"size:16;not null;default:ok"`
	// Period 是当前状态对应的周期（YYYY-MM，UTC）。
	//
	// 跨月时清空状态与下面两个时刻。不记周期的话，上个月被限速的用户在
	// 新的一月里仍然限着——而那时他的用量是 0，界面上显示「已超限」，
	// 没有任何地方能解释这件事。
	Period *string `gorm:"size:7"`

	WarnedAt  *time.Time `gorm:"column:warned_at"`
	LimitedAt *time.Time `gorm:"column:limited_at"`

	// Detail 是最近一次判定的说明。
	Detail *string `gorm:"type:text"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (ResourceQuota) TableName() string { return "resource_quota" }

// Unlimited 报告该配额是否不限制。
func (q *ResourceQuota) Unlimited() bool { return q.LimitValue <= 0 }

// Limited 报告当前是否已处于超限处置中。
func (q *ResourceQuota) Limited() bool { return q.Status == QuotaStatusLimited }

// Unit 返回该维度的展示单位。
func QuotaDimUnit(dim string) string {
	switch dim {
	case QuotaDimTrafficIn, QuotaDimTrafficOut:
		return "GB"
	case QuotaDimRuntime:
		return "小时"
	}
	return ""
}

// QuotaDimLabel 返回该维度的中文名。
func QuotaDimLabel(dim string) string {
	switch dim {
	case QuotaDimTrafficIn:
		return "月入站流量"
	case QuotaDimTrafficOut:
		return "月出站流量"
	case QuotaDimRuntime:
		return "月运行时长"
	}
	return dim
}

// ValidQuotaDim 报告维度是否合法。
func ValidQuotaDim(dim string) bool {
	switch dim {
	case QuotaDimTrafficIn, QuotaDimTrafficOut, QuotaDimRuntime:
		return true
	}
	return false
}

// ValidQuotaAction 报告处置动作是否合法。
func ValidQuotaAction(a string) bool {
	return a == QuotaActionThrottle || a == QuotaActionBlock
}
