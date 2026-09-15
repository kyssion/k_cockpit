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
type StoragePool struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null"`
	// DeviceID 是设备的稳定标识；DevicePath 仅用于展示。
	DeviceID   string  `gorm:"size:128;not null"`
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

	// IsDefault 表示该池是该节点的默认池。每节点至多一个，由数据库的
	// 部分唯一索引 uniq_storage_pool_default 保证。
	IsDefault bool `gorm:"not null;default:false"`

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
