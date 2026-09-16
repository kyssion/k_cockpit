package model

import "time"

// 快照种类。
const (
	// SnapshotKindInternal 保存在磁盘镜像内部。
	//
	// 含内存时可恢复**运行现场**，代价是快照体积可能数倍于磁盘本身，
	// 且对磁盘 I/O 有持续影响。
	SnapshotKindInternal = "internal"
	// SnapshotKindExternal 以 `--disk-only` 方式另存为独立文件，只能恢复磁盘。
	SnapshotKindExternal = "external"
)

// 快照状态。
//
// 创建与恢复都是异步任务，界面需要在这期间展示中间状态——这正是快照必须
// 落库的原因之一：虚拟化层不提供「正在创建」这种状态给我们查。
const (
	SnapshotCreating  = "creating"
	SnapshotReady     = "ready"
	SnapshotRestoring = "restoring"
	SnapshotDeleting  = "deleting"
	SnapshotError     = "error"
)

// VMSnapshot 对应 vm_snapshot 表：一台虚拟机的快照。
//
// 它与 vm 表同样是**投影 + 元数据**的混合体：虚拟化层是权威，本表缓存它
// 并补充控制面自己的信息（归属、描述、任务状态）。因此 status 显示为 ready
// 只代表**上次对账时**它是就绪的。
type VMSnapshot struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"not null;uniqueIndex:uniq_vm_snapshot_vm_name,priority:1;index:idx_vm_snapshot_vm_created,priority:1"`
	NodeID int64 `gorm:"not null"`

	Name        string  `gorm:"size:128;not null;uniqueIndex:uniq_vm_snapshot_vm_name,priority:2"`
	Description *string `gorm:"size:255"`

	Kind          string `gorm:"size:16;not null"`
	IncludeMemory bool   `gorm:"not null;default:false"`

	// VMStatus 是创建快照那一刻虚拟机所处的状态。
	//
	// 恢复时据此判断是否必须先关机：把运行中虚拟机的磁盘状态直接回滚，
	// 得到的是一个**文件系统可能已损坏**的来宾系统——它也许还能启动，
	// 然后在某个随机时刻崩溃，而那时没有任何线索指向这次恢复。
	VMStatus *string `gorm:"size:16"`

	// DomainName 是虚拟化层里的快照标识。
	//
	// 与 Name 分开记录：Name 是用户可见、可改的名字；DomainName 一旦建立
	// 就不该再变，变了就找不到那条快照。把两者合成一个字段，会让「重命名
	// 快照」这个很自然的需求变成一次危险操作。
	DomainName *string `gorm:"size:255"`

	SizeBytes int64 `gorm:"not null;default:0"`

	// ParentID 指向父快照。外部快照会形成链，链上的中间节点被删除会让
	// 子快照失去依赖——因此删除前要检查 HasChildren。
	ParentID *int64

	Status string `gorm:"size:16;not null;default:creating"`

	// HasChildren 表示是否存在子快照。界面据此禁用删除并说明原因，
	// 而不是让用户点下去才收到一个拒绝。
	HasChildren bool `gorm:"not null;default:false"`

	// IsCurrent 表示虚拟机当前是否运行在这个快照上。
	// 为 true 时恢复它没有意义，界面应把「恢复」置灰。
	IsCurrent bool `gorm:"not null;default:false"`

	CreatedBy *int64
	CreatedAt time.Time `gorm:"index:idx_vm_snapshot_vm_created,priority:2"`
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMSnapshot) TableName() string { return "vm_snapshot" }

// IsReady 报告快照是否可用于恢复。
//
// 只有 ready 能恢复：creating 时快照本身还没写完，error 时它不可信。
func (s *VMSnapshot) IsReady() bool {
	return s.Status == SnapshotReady
}

// CanDelete 报告快照是否可以被删除。
//
// 两个条件缺一不可：**没有子快照**（否则会破坏依赖链）、**不是当前快照**
// （删掉它意味着虚拟机正运行在一个已不存在的快照上）。
func (s *VMSnapshot) CanDelete() bool {
	return s.Status != SnapshotCreating && s.Status != SnapshotDeleting &&
		!s.HasChildren && !s.IsCurrent
}
