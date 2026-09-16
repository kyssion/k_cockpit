package model

import "time"

// 存储池状态（f-5-01 R-007）。
const (
	// StoragePoolReady 可用：设备已格式化并挂载。
	StoragePoolReady = "ready"
	// StoragePoolUnmounted 已配置但未挂载。可用但需要先挂载，
	// 界面应给出明确提示而不是把它当作不可用。
	StoragePoolUnmounted = "unmounted"
	// StoragePoolUnallocated 设备存在但尚未分配。
	StoragePoolUnallocated = "unallocated"
	// StoragePoolError 异常，附原因（如设备已不存在）。
	StoragePoolError = "error"
)

// StoragePool 对应 storage_pool 表。
//
// 唯一标识是 `(NodeID, DeviceID)` 而不是设备路径：路径会随插入顺序变化，
// 用它做键会导致同一块盘被识别成两块（f-5-01 R-010）。
// 索引在这里**显式声明**，而不是只写在迁移里。
//
// 测试库由 AutoMigrate 按模型建表：模型不声明，测试库就没有这些约束，于是
// 「两个池同时成为默认池」在测试里畅通无阻，只在真实库上被拦下；而生产里
// 那条路径的兜底逻辑（改成非默认池）也就永远没被测试覆盖过。
//
// 这与列名那次（VCPU → v_cpu、CIDR → c_id_r）是同一个成因：**模型与迁移
// 各说各话，而测试库站在模型这一边**。凡是迁移里有的约束，模型都要有。
type StoragePool struct {
	ID int64 `gorm:"primaryKey"`
	// NodeID 参与两个索引，因此两处都要声明；
	// uniq_storage_pool_node_device 是复合索引，用 priority 指定列序。
	NodeID int64 `gorm:"not null;index:idx_storage_pool_node_id;uniqueIndex:uniq_storage_pool_node_device,priority:1"`
	// DeviceID 是设备的稳定标识；DevicePath 仅用于展示。
	DeviceID   string  `gorm:"size:128;not null;uniqueIndex:uniq_storage_pool_node_device,priority:2"`
	DevicePath *string `gorm:"size:255"`
	// Kind 是存储类型，当前只有 local。
	Kind   string  `gorm:"size:16;not null;default:local"`
	FSType *string `gorm:"size:16"`
	// MountPath 是挂载点。
	MountPath *string `gorm:"size:255"`

	// TotalBytes / UsableBytes 由 agent **随指标周期上报**，不按需实时探测
	// （f-5-01 R-009）：为一个列表页去逐节点探测空间，会让页面在节点多时
	// 变得极慢。代价是数据可能滞后，因此界面必须标注新鲜度。
	TotalBytes  int64 `gorm:"not null;default:0"`
	UsableBytes int64 `gorm:"not null;default:0"`

	// IsDefault 表示该池是该节点的默认池，每节点至多一个（R-006）。
	//
	// where 条件**不能省**：唯一性只针对 is_default = true 的行。少了它，
	// 唯一约束会落到所有行上，每节点就只能存一个**非默认**池——那显然不对，
	// 而且会在第二个池创建时以一个看不懂的冲突暴露出来。
	IsDefault bool `gorm:"not null;default:false;uniqueIndex:uniq_storage_pool_default,where:is_default"`

	Status string  `gorm:"size:16;not null;default:ready"`
	Remark *string `gorm:"size:255"`

	// LastReportedAt 是空间数据的最近上报时间，用于计算新鲜度。
	LastReportedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt 为软删除：被删除的存储池仍需保留在库中供审计引用。
	DeletedAt *time.Time
}

// TableName 固定表名。
func (StoragePool) TableName() string { return "storage_pool" }

// IsStale 报告空间数据是否可能已经过期。
//
// 超过阈值未上报时界面应提示——显示一个陈旧的容量比不显示更危险：
// 用户可能基于「还有 500G」去创建虚拟机，而实际早已写满（f-5-01 边界）。
func (p *StoragePool) IsStale(now time.Time, threshold time.Duration) bool {
	if p.LastReportedAt == nil {
		return true
	}
	return now.Sub(*p.LastReportedAt) > threshold
}
