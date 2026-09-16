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

	// --- 控制台配置（f-2-08）---

	// VNCEnabled 表示控制台是否开启。
	VNCEnabled bool `gorm:"not null;default:false"`
	// VNCPort 是宿主上的 VNC 端口。它**只监听 VNCBind 指定的地址**。
	VNCPort *int
	// VNCBind 是监听地址，默认 127.0.0.1（R-001）。
	//
	// 宿主机上不开放对外端口是默认状态；改变它需要显式开启「对外暴露」，
	// 而那是一个需要二次验证的高危操作（R-004）。
	VNCBind string `gorm:"size:64;not null;default:127.0.0.1"`
	// VNCExposed 表示控制台端口是否已对外暴露。
	//
	// 与 VNCBind 分开记录，而不是从监听地址推断：让「曾经暴露过」这件事
	// 在界面上与审计里都留下明确痕迹。
	VNCExposed bool `gorm:"not null;default:false"`
	// DisplayDevice 是显示设备类型；取值 `none` 表示该虚拟机**没有控制台**
	// （R-011）。界面据此隐藏入口，而不是给用户一个点了打不开的按钮。
	DisplayDevice string `gorm:"size:16;not null;default:vnc"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// 显示设备类型。
const (
	// DisplayVNC 有图形控制台。
	DisplayVNC = "vnc"
	// DisplayNone 无控制台：虚拟机以串口或纯命令行方式运行，
	// 没有可显示的画面（R-011）。
	DisplayNone = "none"
)

// HasConsole 报告该虚拟机是否有可用的控制台。
func (v *VM) HasConsole() bool {
	return v.DisplayDevice != DisplayNone
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
