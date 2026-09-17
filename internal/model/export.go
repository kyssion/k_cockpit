package model

import (
	"time"

	"gorm.io/gorm"
)

// 导出状态。与 task.status 用同一套词汇，避免界面上出现两种说法指着同一件事。
const (
	ExportPending = "pending"
	ExportRunning = "running"
	ExportSuccess = "success"
	ExportFailed  = "failed"
)

// 导出格式（f-2-14）。
const (
	// ExportQCOW2 只导出系统盘，得到一个可直接被 QEMU 使用的镜像。
	ExportQCOW2 = "qcow2"
	// ExportOVA 导出为标准 OVA 包（含 OVF 描述），可被其它虚拟化平台导入。
	//
	// 它比裸镜像大：OVF 描述、校验清单这些"包装"也要一并写进去。对用户来说
	// 值得的差别是**可移植性**——裸镜像只有懂 QEMU 的人能用，OVA 可以被
	// VirtualBox、ESXi 之类直接导入。
	ExportOVA = "ova"
)

// VMExport 对应 vm_export 表：一次导出的产物记录（F-2-14）。
//
// 独立于任务存在：产物是长期的东西（用户可以下载、可以删除），而任务记录
// 是可以被清理的运维数据。把产物挂在任务上，等于让「三个月前导出的那个
// 镜像」随着任务清理一起消失。
type VMExport struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"not null;index:idx_vm_export_vm_id"`
	NodeID int64 `gorm:"not null"`
	// VMName 冗余保存：虚拟机被删除后，导出记录仍要能说明它来自哪里。
	// 只留一个 vm_id 的话，那条记录就变成了一串无意义的数字。
	VMName string `gorm:"size:128"`

	Format string `gorm:"size:16;not null;default:qcow2"`
	// IncludeDataDisks 表示是否连同数据盘一起导出。
	//
	// **默认不包含**（f-2-14）：数据盘可能远大于系统盘，而多数导出是为了
	// 复用系统环境（做模板、迁移到别处），不是搬数据。
	IncludeDataDisks bool `gorm:"not null;default:false"`

	Status string `gorm:"size:16;not null;default:pending"`

	// FilePath 由节点在导出完成后返回——控制面只保存不解释：
	// 它可能落在某个存储池的导出目录里，具体在哪由实现决定。
	FilePath *string `gorm:"size:512"`
	// FileName 是提供给用户的下载文件名，含扩展名。
	FileName *string `gorm:"size:255"`
	// SizeBytes 是产物大小。它**计入用户的存储配额**（f-2-14），
	// 因此必须在导出完成后如实记录——记账用 0 会让配额永远不算这份占用。
	SizeBytes int64 `gorm:"not null;default:0"`

	Error     *string `gorm:"size:512"`
	CreatedBy *int64

	CreatedAt  time.Time
	FinishedAt *time.Time
	// 软删除：删除产物后记录保留，供审计与配额核销追溯。
	DeletedAt gorm.DeletedAt
}

// TableName 固定表名。
func (VMExport) TableName() string { return "vm_export" }

// IsRunning 报告导出是否仍在进行中。
func (e *VMExport) IsRunning() bool {
	return e.Status == ExportPending || e.Status == ExportRunning
}
