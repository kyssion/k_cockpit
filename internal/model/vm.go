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
	ID int64 `gorm:"primaryKey"`
	// NodeID 参与两个唯一索引（名称与 UUID 都在节点内唯一），两处都要声明。
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_vm_node_name,priority:1;uniqueIndex:uniq_vm_node_uuid,priority:1"`
	// Name 在节点内唯一（uniq_vm_node_name）；跨节点可以重名。
	//
	// 索引**必须在模型上声明**，不能只写在迁移里：模型不声明，测试库就没有
	// 这条约束，于是「同名虚拟机」在测试里能建两个、真实库上却会失败——
	// 而这条路径恰恰是最常见的一类冲突。详见 model/storage.go 的同款说明。
	Name string `gorm:"size:63;not null;uniqueIndex:uniq_vm_node_name,priority:2"`
	// UUID 同样**在节点内唯一**，但可为空（投影尚未同步到时）。
	// 空值不参与唯一性判定，因此多台未同步的虚拟机不会互相冲突。
	UUID *string `gorm:"size:64;uniqueIndex:uniq_vm_node_uuid,priority:2"`

	OwnerID    *int64
	TemplateID *int64

	Status string `gorm:"size:16;not null;default:unknown"`

	// column 必须显式声明：GORM 的命名策略按大写字母边界切分，
	// `VCPU` 会被转成 `v_cpu`，而迁移里建的列叫 `vcpu`。
	//
	// 这类不一致在测试里**看不出来**——测试库由 AutoMigrate 按同一套策略
	// 建表，两边一起错；只有连上由 SQL 迁移建的真实库才会暴露，且表现为
	// 一个笼统的「服务内部错误」。凡是连续大写的缩写字段都要显式声明列名。
	VCPU      int     `gorm:"column:vcpu;not null;default:0"`
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

	// --- 启动与安全（f-2-05「启动与安全」子选项卡）---
	//
	// 下面这批字段的 column 全部**显式声明**：GORM 的命名策略按大写字母
	// 边界切分，`OSType` 会被转成 `o_s_type`、`APIC` 转成 `a_p_i_c`——
	// 而迁移里建的列是 `os_type` / `apic`。这类不一致不编译报错、测试也
	// 发现不了（测试库由同一套策略建表，两边一起错），只在连上真实库时
	// 表现为一个笼统的 500。本项目为此踩过一次（VCPU → v_cpu）。

	OSType      string `gorm:"column:os_type;size:32;not null;default:linux"`
	MachineType string `gorm:"size:32;not null;default:q35"`
	Firmware    string `gorm:"size:16;not null;default:bios"`
	SecureBoot  bool   `gorm:"not null;default:false"`
	BootOrder   string `gorm:"size:128;not null;default:disk,cdrom,network"`
	AutoStart   bool   `gorm:"not null;default:false"`
	Watchdog    string `gorm:"size:16;not null;default:none"`

	// --- 高级设置（f-2-05「高级设置」子选项卡）---

	CPUType         string `gorm:"column:cpu_type;size:32;not null;default:host"`
	CPULimitPercent int    `gorm:"column:cpu_limit_percent;not null;default:0"`
	MemoryHugepages bool   `gorm:"not null;default:false"`
	APIC            bool   `gorm:"column:apic;not null;default:true"`
	PAE             bool   `gorm:"column:pae;not null;default:true"`
	// GuestAgent 是**探测结果**而非配置：由 agent 上报「来宾里是否运行了
	// QEMU Guest Agent」。放在这里是因为「来宾自动化」（f-2-10）的每项能力
	// 都以它为前置，界面上需要有地方显示这个状态。
	GuestAgent    bool   `gorm:"not null;default:false"`
	InitMode      string `gorm:"size:32;not null;default:none"`
	FreezeOnStart bool   `gorm:"not null;default:false"`

	// --- 磁盘（f-2-05「磁盘与驱动器」子选项卡）---

	DiskFormat string `gorm:"size:16;not null;default:qcow2"`
	// IOPS 限制：总量与读写分离**互斥**（f-2-06）。
	//
	// 三组值都保留在表里：「互斥」是业务规则，由服务层校验；用「表里只存
	// 一种」来表达它，会让用户在两种模式之间来回切换时丢失另一组已设好的值。
	DiskIOPSTotal int `gorm:"column:disk_iops_total;not null;default:0"`
	DiskIOPSRead  int `gorm:"column:disk_iops_read;not null;default:0"`
	DiskIOPSWrite int `gorm:"column:disk_iops_write;not null;default:0"`

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
