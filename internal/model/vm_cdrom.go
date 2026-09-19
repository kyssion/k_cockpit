package model

import "time"

// 光驱的总线类型。
//
// 默认 sata：它在绝大多数来宾里都能被识别，而且支持热插拔。ide 出现在老系统
// 里（而**换到 ide 或 scsi 几乎一定要重启**），scsi 需要来宾里有驱动。
const (
	CDROMBusIDE  = "ide"
	CDROMBusSATA = "sata"
	CDROMBusSCSI = "scsi"
)

// ValidCDROMBus 报告总线类型是否合法。
func ValidCDROMBus(b string) bool {
	return b == CDROMBusIDE || b == CDROMBusSATA || b == CDROMBusSCSI
}

// VMCDROM 对应 vm_cdrom 表：虚拟机的一个光驱。
//
// **做成一行的表而不是 VM 上的三个字段**，理由不是"更接近 libvirt"，而是那
// 三个字段表达不了顺序：多个光驱必须有稳定的编号，否则加一个、删一个之后
// **设备号会漂**——来宾里 /dev/sr0 与 /dev/sr1 对调了，而用户按上次记的
// 设备名去找会找错。
type VMCDROM struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"column:vm_id;not null;uniqueIndex:uniq_vm_cdrom_vm_order,priority:1"`
	NodeID int64 `gorm:"column:node_id;not null;index:idx_vm_cdrom_node"`

	// OrderNo 是光驱序号（从 0 开始），在**同一台虚拟机内唯一**。
	//
	// 它是设备号的来源——来宾里看到的就是按这个顺序排的 /dev/srN。
	OrderNo int `gorm:"column:order_no;not null;uniqueIndex:uniq_vm_cdrom_vm_order,priority:2"`

	// ISOFileID 指向 storage_file（category = iso）。
	//
	// 为空表示**光驱在但没放盘**——即"弹出"的状态。它与"没有光驱"是两件
	// 事：前者在来宾里看得到一个空的托盘，后者连设备都没有。混为一谈的话，
	// 用户在来宾里找不到设备而不知道是哪一种。
	ISOFileID *int64 `gorm:"column:iso_file_id"`

	// Bus 取值 ide / sata / scsi（见上面的常量）。
	Bus string `gorm:"size:8;not null;default:sata"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMCDROM) TableName() string { return "vm_cdrom" }

// Loaded 报告这个光驱里是否放了盘。
func (c *VMCDROM) Loaded() bool { return c.ISOFileID != nil }
