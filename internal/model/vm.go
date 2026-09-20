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

	OwnerID *int64
	// TemplateID 指向制备这台虚拟机所用的模板；为空表示从零安装或来源已删。
	TemplateID *int64 `gorm:"index:idx_vm_template_id"`

	// CloneMode 取值 full / linked（见 model.CloneFull / CloneLinked）。
	//
	// 它不只是个标签：完整克隆与模板**完全独立**，链式克隆的磁盘只是一个
	// overlay——**父盘缺失或被改，数据就不可用了**。界面据此显示依赖链，
	// 删除父模板时也据此拒绝或明确告知会破坏多少个克隆体。
	CloneMode string `gorm:"size:16;not null;default:full"`
	// BackingPath 是链式克隆的父磁盘路径，由节点在克隆时返回。
	//
	// 没有它就无法回答「这个克隆体依赖谁」——而那是排查「虚拟机起不来」
	// 时第一个要看的东西。控制面只保存不解释这个路径。
	BackingPath *string `gorm:"size:512"`

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
	// ⚠️ **GORM 陷阱**（同 model/schedule.go 的 Enabled）：`default:true` 时
	// 值为 `false` 会被当作零值省略，数据库填入 `true`。
	//
	// 虚拟机的 present 只在**删除任务**里被改成 false，走的是 Update
	// （显式列名），因此不受影响。
	Present bool `gorm:"not null;default:true"`
	// LastSyncedAt 是最近一次与虚拟化层对账的时间，用于计算数据新鲜度。
	LastSyncedAt *time.Time
	// DeletedAt 是**移入回收站**的时刻，为空表示不在回收站里。
	//
	// 这里刻意**不用** gorm.DeletedAt：那个类型会让 GORM 给所有查询自动加上
	// `deleted_at IS NULL`，于是不在列表里的机器在回收站、审计、历史任务里
	// 也都查不到——而我们恰恰需要它们。是否"可见"由 present 决定，这一列
	// 只回答"什么时候删的"。
	DeletedAt *time.Time

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

	// --- SPICE 控制台（F-2-09）---
	//
	// 与 VNC **平行而不是合并**：同一台机器可以两种控制台都开，而它们的
	// 对外暴露是两件独立的事（关掉 SPICE 的暴露不该影响 VNC 的监听地址）。
	//
	// 但**暴露的判定与文案是共用的**（见 vm.console）：另写一套的话，
	// 某天有人给 VNC 那条加了更严的限制，而 SPICE 那条还开着——那种分叉
	// 不会以任何形式报错。
	// SPICESupported 由**节点探测**填入：SPICE 是 libvirt 编译期的可选项，
	// 而 VNC 几乎总是可用。不探测的话，界面上会出现一个点了打不开的
	// SPICE 选项——用户会去反复检查"是不是我哪里配错了"。
	SPICESupported bool   `gorm:"column:spice_supported;not null;default:false"`
	SPICEEnabled   bool   `gorm:"column:spice_enabled;not null;default:false"`
	SPICEPort      *int   `gorm:"column:spice_port"`
	SPICEBind      string `gorm:"column:spice_bind;size:64;not null;default:127.0.0.1"`
	SPICEExposed   bool   `gorm:"column:spice_exposed;not null;default:false"`

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
	// APIC / PAE 同样是 default:true 的字段：创建时如果传 false 会被省略
	// 而变成 true。当前它们只出现在编辑矩阵（走 Update），不受影响。
	APIC bool `gorm:"column:apic;not null;default:true"`
	PAE  bool `gorm:"column:pae;not null;default:true"`
	// GuestAgent 是**探测结果**而非配置：由 agent 上报「来宾里是否运行了
	// QEMU Guest Agent」。放在这里是因为「来宾自动化」（f-2-10）的每项能力
	// 都以它为前置，界面上需要有地方显示这个状态。
	GuestAgent    bool   `gorm:"not null;default:false"`
	InitMode      string `gorm:"size:32;not null;default:none"`
	FreezeOnStart bool   `gorm:"not null;default:false"`

	// --- 磁盘（f-2-05「磁盘与驱动器」子选项卡）---

	DiskFormat string `gorm:"size:16;not null;default:qcow2"`
	// DiskBus 是系统盘的驱动类型（f-2-02 创建向导）。
	//
	// 存成列而不是只在创建时用一次：它描述的是「这台机器用哪种磁盘控制器」
	// 这个**事实**，详情页与编辑页都要显示它。只留在创建参数里的话，事后
	// 想换总线就没有依据可改（换总线属于 f-2-06 的能力）。
	DiskBus string `gorm:"size:16;not null;default:virtio"`
	// NicModel 是创建时的网卡型号，也是之后新增网口的默认值。
	NicModel string `gorm:"size:16;not null;default:virtio"`
	// IOPS 限制：总量与读写分离**互斥**（f-2-06）。
	//
	// 三组值都保留在表里：「互斥」是业务规则，由服务层校验；用「表里只存
	// 一种」来表达它，会让用户在两种模式之间来回切换时丢失另一组已设好的值。
	DiskIOPSTotal int `gorm:"column:disk_iops_total;not null;default:0"`
	DiskIOPSRead  int `gorm:"column:disk_iops_read;not null;default:0"`
	DiskIOPSWrite int `gorm:"column:disk_iops_write;not null;default:0"`

	// --- 救援系统（F-2-12）---

	// RescueActive 表示该虚拟机当前处于救援模式。
	RescueActive bool `gorm:"not null;default:false"`
	// RescueConfig 是**进入救援之前**的那份配置快照（JSON 文本）。
	//
	// 没有它，退出救援时只能猜一个默认值填回去——而那是悄悄改掉用户的配置：
	// 他进入救援是为了修系统，退出后发现引导顺序被重置成了默认值，机器起不来了。
	//
	// 用 JSON 而不是拆成多列：快照的字段集合会随救援能力扩展而变化（将来
	// 可能要改 BIOS、CPU 型号），拆列意味着每加一项都要一次迁移，而这份数据
	// 只是「原样存、原样还」，控制面从不按字段查询它。
	RescueConfig *string `gorm:"type:text"`
	// RescueSince 是进入救援的时刻。
	RescueSince *time.Time

	// --- 重装系统（F-2-11）---

	// ReinstallBackup 是重装时留下的**原系统盘备份**路径。
	//
	// 它有两个用途：重装过程中任何一步失败时还原；以及重装**成功之后**继续
	// 存在——那是用户的数据，什么时候回收由他决定，系统不能替他做主删掉。
	//
	// 只有一份：第二次重装会覆盖上一次的。服务层会**显式拒绝**而不是静默
	// 覆盖——静默覆盖会让用户失去「回到上一个系统」这个唯一的退路。
	ReinstallBackup *string `gorm:"size:512"`
	// ReinstallAt 是最近一次重装的时刻。
	ReinstallAt *time.Time

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
