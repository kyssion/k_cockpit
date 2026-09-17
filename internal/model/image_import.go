package model

import (
	"time"

	"gorm.io/gorm"
)

// 导入状态。与 task.status 用同一套词汇。
const (
	ImportPending = "pending"
	ImportRunning = "running"
	ImportSuccess = "success"
	ImportFailed  = "failed"
)

// 可导入的源格式（f-2-13）。
const (
	ImportQCOW2 = "qcow2"
	ImportRaw   = "raw"
	ImportVMDK  = "vmdk"
	ImportVHD   = "vhd"
	ImportVHDX  = "vhdx"
	ImportIMG   = "img"
	// ImportOVA 是打包格式：除了磁盘，还带 OVF 描述与校验清单。
	//
	// 它与其它几种**不是同一类东西**：其余都是裸磁盘镜像，而 OVA 里带着
	// 一份机器描述（CPU、内存、磁盘、网络）。因此 OVA 可以「先解析预览」，
	// 把那份描述展示给用户确认；裸镜像没有可预览的内容，只能问用户想怎么配。
	ImportOVA = "ova"
)

// ImageImport 对应 image_import 表：一次磁盘/镜像导入（F-2-13）。
//
// 产物是**一个模板**，不是一台虚拟机。导入一份别人的镜像之后，用户接下来
// 多半是「用它开几台机」，而不是「就要这一台」——产出模板把人带到已有的
// 克隆流程上，也顺便获得了模板那套可见性与依赖管理。
type ImageImport struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;index:idx_image_import_node_id"`
	// Name 是导入后模板的名字。
	Name string `gorm:"size:64;not null"`

	SourceFilename string `gorm:"size:255;not null"`
	// SourceFormat 是**转换前**的原始格式。
	//
	// 导入过程中会被统一转成 qcow2，但原始格式要留档——「导进来的盘当初是
	// 什么格式」在排查转换相关问题时是第一条线索。
	SourceFormat    string `gorm:"size:16;not null;default:qcow2"`
	SourceSizeBytes int64  `gorm:"not null;default:0"`

	Status string `gorm:"size:16;not null;default:pending"`

	DiskPath   *string `gorm:"size:512"`
	TemplateID *int64
	// Preview 是解析出来的配置预览（JSON 文本）。
	//
	// 留存下来是为了让「用户按下确认时看到的东西」与「真正被创建的东西」
	// 是同一份——只在前端存着的话，刷新页面就没了，而事后也无法回答
	// 「当初预览里写的是什么」。
	Preview *string `gorm:"type:text"`

	Error     *string `gorm:"size:512"`
	CreatedBy *int64

	CreatedAt  time.Time
	FinishedAt *time.Time
	// 软删除：产物模板被引用（克隆出来的虚拟机），记录要留。
	DeletedAt gorm.DeletedAt
}

// TableName 固定表名。
func (ImageImport) TableName() string { return "image_import" }

// IsBundled 报告该来源是否是打包格式（OVA）。
//
// 只有它带机器描述，因此只有它能「先解析预览」。
func (i *ImageImport) IsBundled() bool { return i.SourceFormat == ImportOVA }

// ImportPreview 是解析出来的配置预览（f-2-13）。
//
// 对 OVA 而言这些值来自包内的 OVF 描述；对裸镜像而言它们是**用户填的**，
// 因此 Sources 字段会说明每一项的来源——用户需要知道「这个 4 核是我自己
// 选的，还是从包里读出来的」，两者对错误的含义完全不同。
type ImportPreview struct {
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	OSType    string `json:"os_type,omitempty"`
	OSVariant string `json:"os_variant,omitempty"`
	// Sources 说明每个字段的来源：`ovf`（从包里解析）或 `user`（用户填写）。
	Sources map[string]string `json:"sources"`
	// Notes 是解析过程中的提示，例如「磁盘格式为 vmdk，导入时会自动转换」。
	Notes []string `json:"notes,omitempty"`
}

// ImportFormatLabel 把格式翻译成用户能读懂的说法。
func ImportFormatLabel(format string) string {
	switch format {
	case ImportQCOW2:
		return "QCOW2 镜像"
	case ImportRaw:
		return "RAW 裸镜像"
	case ImportVMDK:
		return "VMDK（VMware）"
	case ImportVHD, ImportVHDX:
		return "VHD/VHDX（Hyper-V）"
	case ImportIMG:
		return "IMG 镜像"
	case ImportOVA:
		return "OVA 包（含机器描述）"
	default:
		return format
	}
}
