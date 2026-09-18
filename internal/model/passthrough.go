package model

import "time"

// VMPassthrough 对应 vm_passthrough 表：一台虚拟机挂载的一块直通设备。
//
// **设备清单本身不在库里**——它来自节点探测，而设备可热插拔、绑定状态也会
// 被宿主机上的人手工改动。存一份会出现"库里有、机器上没有"的分歧，而那种
// 分歧不会报错，只会在用户点"挂载"时以一个看不懂的理由失败。
//
// 因此本表只存**控制面的意图**：哪个虚拟机挂了哪个 PCI 设备。
type VMPassthrough struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"column:vm_id;not null;uniqueIndex:uniq_vm_passthrough_vm_addr,priority:1"`
	NodeID int64 `gorm:"column:node_id;not null;index:idx_vm_passthrough_node"`

	// PCIAddress 是设备在宿主机上的 PCI 地址（如 0000:01:00.0）。
	//
	// 它是**在宿主机上定位设备的唯一标识**，而 vendor:device 不是——同一台
	// 机器上可以有两块完全相同的卡，它们的直通分组与槽位都不同。
	PCIAddress string `gorm:"column:pci_address;size:32;not null;uniqueIndex:uniq_vm_passthrough_vm_addr,priority:2"`

	// DeviceDesc 是挂载时的设备描述，**冗余存一份**。
	//
	// 设备被拔掉或换到别的槽位之后，探测结果里就没有它了。那时用户需要知道
	// "这台机器挂了什么"，而回查探测结果查不到——设备已经不在了。
	// 「当时挂的是什么」是历史的一部分。
	DeviceDesc *string `gorm:"column:device_desc;size:255"`

	// IOMUGroup 同样是快照，用于排查"当初是不是因为同组冲突"。
	//
	// 分组会随硬件与拓扑变化，因此它不能当作当前值使用，只能当作历史。
	IOMUGroup *int `gorm:"column:iommu_group"`

	Remark *string `gorm:"size:255"`

	// 不加 `default:now()`：迁移里的默认值是真的（数据库侧兜底），但模型上
	// 写它会让 AutoMigrate 在 SQLite 下建不出表（`now()` 不是 SQLite 的默认值
	// 写法），而测试库是按模型建的。时间统一由 Go 侧赋值。
	AttachedAt time.Time `gorm:"column:attached_at;not null"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// TableName 固定表名。
func (VMPassthrough) TableName() string { return "vm_passthrough" }
