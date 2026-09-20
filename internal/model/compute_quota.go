package model

import "time"

// ComputeQuota 对应 compute_quota 表：一个用户在一个节点上的**计算资源上限**。
//
// 它刻意**不与 resource_quota 共用一张表**，尽管形状相似：
//
//   - resource_quota 是**周期累计型**（这个月用了多少流量 / 多少小时），
//     有"当前周期""跨月重置""超限后限速或断网"这一整套语义；
//   - 本表是**存量型**（此刻占着几个核、几 GB、几台），没有周期，超限也
//     不是"限速"——而是**拒绝新建**。
//
// 混进同一张表的话，周期评估循环会按月重置这些上限（明明是长期有效的
// 约束），而"处置"那一列也得为它塞一个既不是限速也不是断网的值。那种
// 表读起来每一列都有例外。
type ComputeQuota struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"column:node_id;not null;uniqueIndex:uniq_compute_quota_node_user,priority:1"`
	UserID int64 `gorm:"column:user_id;not null;uniqueIndex:uniq_compute_quota_node_user,priority:2"`

	// VCPU / MemoryMB / VMCount 是上限。**0 表示不限**。
	//
	// 三个维度放在一行而不是按维度分三行：它们总是**一起设置**（"给这个
	// 用户 8 核 / 16 GB / 5 台"），分行的唯一结果是三处各自可能只填了一半，
	// 而那时"这个用户到底能建几台"要遍历三行才知道。
	VCPU     int `gorm:"column:vcpu;not null;default:0"`
	MemoryMB int `gorm:"column:memory_mb;not null;default:0"`
	VMCount  int `gorm:"column:vm_count;not null;default:0"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (ComputeQuota) TableName() string { return "compute_quota" }
