package model

import "time"

// 虚拟机状态。
//
// 这是**投影字段**：描述虚拟化层的真实状态，由 agent 上报后写入。
// 它不参与业务判定——判定以虚拟化层为准，投影只用于列表展示与刷新。
const (
	VMStatusRunning   = "running"
	VMStatusStopped   = "stopped"
	VMStatusPaused    = "paused"
	VMStatusSuspended = "suspended"
	VMStatusError     = "error"
	VMStatusUnknown   = "unknown"
)

// VM 对应 vm 表。
//
// 这张表混合了两类数据，理解它们的来源差异是读懂本结构的前提：
//
//   - **投影字段**（Status / VCPU / MemoryMB / DiskGB / IPSummary）的权威在
//     虚拟化层，控制面只是缓存，可能陈旧；
//   - **元数据字段**（OwnerID / Remark / GroupName）的权威在控制面。
//
// 因此 `status` 显示为 stopped 不代表虚拟机真的停了——它只代表**上次对账时**
// 它是停止的（见 LastSyncedAt 与 f-2-01 的新鲜度口径）。
type VM struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null"`
	// Name 在节点内唯一（uniq_vm_node_name）；跨节点可以重名。
	Name string  `gorm:"size:63;not null"`
	UUID *string `gorm:"size:64"`

	OwnerID    *int64
	TemplateID *int64

	Status    string  `gorm:"size:16;not null;default:unknown"`
	VCPU      int     `gorm:"not null;default:0"`
	MemoryMB  int     `gorm:"not null;default:0"`
	DiskGB    int     `gorm:"not null;default:0"`
	IPSummary *string `gorm:"size:255"`

	Remark    *string `gorm:"size:200"`
	GroupName *string `gorm:"size:64"`

	// Present 表示虚拟化层是否仍存在该虚拟机。
	//
	// 为 false 说明它在面板之外被删除了。界面应把它标记为「已失效」而不是
	// 直接隐藏——直接消失会让用户以为自己误删了。
	Present bool `gorm:"not null;default:true"`
	// LastSyncedAt 是最近一次与虚拟化层对账的时间，用于计算数据新鲜度。
	LastSyncedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VM) TableName() string { return "vm" }

// IsStale 报告投影数据是否已经过期。
//
// 超过阈值未对账时，界面应显示「数据可能陈旧」——把陈旧数据显示成
// 当前状态，在排障场景下比没有数据更危险（f-2-01 Q-004）。
func (v *VM) IsStale(now time.Time, threshold time.Duration) bool {
	if v.LastSyncedAt == nil {
		return true
	}
	return now.Sub(*v.LastSyncedAt) > threshold
}
