package vm

import (
	"context"
	"errors"
	"log"
	"reflect"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// 类型转换的内部错误。它们只用于在 coerceField 内部传递「转换失败」这个事实，
// 从不到达用户——对外一律给带字段名的可读文案。
var (
	errNotInteger = errors.New("not an integer")
	errNotBool    = errors.New("not a boolean")
)

// 子选项卡标识，与 FRONTEND.md §5.3.3 的「编辑」子选项卡对应。
const (
	EditGroupBasic    = "basic"
	EditGroupHardware = "hardware"
	EditGroupDisk     = "disk"
	EditGroupBoot     = "boot"
	EditGroupNetwork  = "network"
	EditGroupPassthru = "passthru"
	EditGroupAdvanced = "advanced"
)

// 控件类型。
const (
	EditKindText    = "text"
	EditKindNumber  = "number"
	EditKindBoolean = "boolean"
	EditKindSelect  = "select"
)

// EditOption 是枚举字段的一个可选值。
type EditOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// EditField 描述一个可编辑的配置项。
type EditField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Kind 决定界面用什么控件：text / number / boolean / select。
	Kind string `json:"kind"`
	// Group 是所属的子选项卡。
	Group string `json:"group"`

	// RequiresNode 表示修改这一项需要**下发到节点**。
	//
	// 为 false 的是纯控制面元数据（备注、分组）：虚拟化层根本不知道有这两个
	// 概念，改它们不需要任何节点操作，因此也**不受运行态限制**——
	// 虚拟机开着也能改备注。
	RequiresNode bool `json:"requires_node"`

	// RequiresShutdown 表示修改这一项必须先关机。
	// 仅在 RequiresNode 为 true 时有意义。
	RequiresShutdown bool `json:"requires_shutdown"`

	// ReadOnly 表示这一项只展示、不可编辑。
	//
	// 用于**探测结果**（如 Guest Agent 是否在运行）：它不是用户能设定的东西，
	// 把它做成可编辑的输入框会让人以为「勾上它就能让 Guest Agent 跑起来」。
	ReadOnly bool `json:"read_only"`

	// InCreate 表示这一项出现在**创建向导**里（f-2-02）。
	//
	// 创建与编辑共用同一份矩阵：它们的规则是同一套（取值、范围、含义），
	// 拆成两份之后「创建时允许的组合」与「编辑时允许的组合」迟早分叉，
	// 而那种分叉只在用户按向导填完、提交被拒时才暴露。
	InCreate bool `json:"in_create"`
	// CreateOnly 表示这一项**只在创建时出现**（编辑页不渲染）。
	//
	// 典型是系统盘容量：创建后改它要走扩容任务（f-2-06），而不是在编辑
	// 表单里改一个数字——后者会让用户以为「改成 20 就能缩到 20」。
	CreateOnly bool `json:"create_only"`
	// Default 是创建向导里的初始值。
	Default string `json:"default,omitempty"`

	Options []EditOption `json:"options,omitempty"`

	Min  int    `json:"min,omitempty"`
	Max  int    `json:"max,omitempty"`
	Hint string `json:"hint,omitempty"`
}

// editFields 是**运行态可改矩阵**（F-2-05）。
//
// 它是这个功能的**单一事实来源**：界面按它渲染控件与「即时生效 / 需关机」
// 标记，后端按它校验提交。前后端各写一份的话，用户会在界面上看到「可热改」、
// 提交后却被后端以「需要关机」拒绝——这类不一致最伤信任，而且只有真正动手
// 操作的用户才会碰到，测试很难覆盖。
var editFields = []EditField{
	// --- 基础配置 ---
	{
		Key: "remark", Label: "备注", Kind: EditKindText, Group: EditGroupBasic,
		// 纯元数据：只存在于控制面，改它不需要碰虚拟化层，因此没有 RequiresShutdown。
		RequiresNode: false, InCreate: true,
	},
	{
		Key: "group_name", Label: "分组", Kind: EditKindText, Group: EditGroupBasic,
		RequiresNode: false, InCreate: true,
	},

	// --- 硬件规格 ---
	//
	// vCPU 与内存单独成组而不是留在「基础配置」里：它们与备注、分组不是
	// 一类东西——前者受运行态约束（要关机才能改），后者随时可改。混在一组
	// 会让用户以为「改个备注也要关机」。
	{
		Key: "vcpu", Label: "CPU 核数", Kind: EditKindNumber, Group: EditGroupHardware,
		RequiresNode: true, RequiresShutdown: true, Min: 1, Max: 64,
		InCreate: true, Default: "2",
		Hint: "需要关机后修改。",
	},
	{
		Key: "memory_mb", Label: "内存（MB）", Kind: EditKindNumber, Group: EditGroupHardware,
		RequiresNode: true, RequiresShutdown: true, Min: 128, Max: 262144,
		InCreate: true, Default: "2048",
		Hint: "需要关机后修改。",
	},

	// --- 磁盘与驱动器 ---
	{
		// 只在创建时出现：之后改容量要走扩容任务（f-2-06），而不是在表单
		// 里改一个数字——后者会让用户以为「改成 20 就能缩到 20」。
		Key: "disk_gb", Label: "系统盘（GB）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, CreateOnly: true, InCreate: true, Default: "40",
		Min: 5, Max: 2000,
		Hint: "按**配置容量**计入存储配额，而不是实际占用。",
	},
	{
		Key: "disk_format", Label: "磁盘格式", Kind: EditKindSelect, Group: EditGroupDisk,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "qcow2",
		Options: []EditOption{
			{Value: "qcow2", Label: "qcow2（支持快照与精简置备）"},
			{Value: "raw", Label: "raw（性能略好，不支持内部快照）"},
		},
		Hint: "转换磁盘格式需要对整个镜像做一次重写。",
	},
	{
		Key: "disk_bus", Label: "磁盘驱动", Kind: EditKindSelect, Group: EditGroupDisk,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "virtio",
		Options: []EditOption{
			{Value: "virtio", Label: "VirtIO（半虚拟化，性能最好）"},
			{Value: "scsi", Label: "SCSI（可热插拔）"},
			{Value: "sata", Label: "SATA（兼容性好）"},
			{Value: "ide", Label: "IDE（老系统兼容）"},
		},
		Hint: "Windows 安装盘通常不带 VirtIO 驱动，需先加载驱动或选 SATA。",
	},
	{
		Key: "disk_iops_total", Label: "IOPS 上限（总量）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, Min: 0, Max: 1000000, InCreate: true, Default: "0",
		Hint: "0 表示不限制。与读写分离的限值**互斥**，两者只能设一组。",
	},
	{
		Key: "disk_iops_read", Label: "IOPS 上限（读）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, Min: 0, Max: 1000000, InCreate: true, Default: "0",
		Hint: "与「总量」互斥。",
	},
	{
		Key: "disk_iops_write", Label: "IOPS 上限（写）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, Min: 0, Max: 1000000, InCreate: true, Default: "0",
		Hint: "与「总量」互斥。",
	},

	{
		Key: "disk_bytes_total", Label: "吞吐上限（总量）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, Min: 0, Max: 100000, InCreate: true, Default: "0",
		Hint: "单位 MB/s，0 表示不限制。与读写分离的限值**互斥**。",
	},
	{
		Key: "disk_bytes_read", Label: "吞吐上限（读）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, Min: 0, Max: 100000, InCreate: true, Default: "0",
		Hint: "单位 MB/s。与「总量」互斥。",
	},
	{
		Key: "disk_bytes_write", Label: "吞吐上限（写）", Kind: EditKindNumber, Group: EditGroupDisk,
		RequiresNode: true, Min: 0, Max: 100000, InCreate: true, Default: "0",
		Hint: "单位 MB/s。与「总量」互斥。",
	},

	// --- 网络设置 ---
	{
		Key: "nic_model", Label: "网卡型号", Kind: EditKindSelect, Group: EditGroupNetwork,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "virtio",
		Options: []EditOption{
			{Value: "virtio", Label: "VirtIO（半虚拟化，性能最好）"},
			{Value: "e1000", Label: "e1000（Intel 千兆，兼容性最好）"},
			{Value: "rtl8139", Label: "rtl8139（老旧系统兼容）"},
		},
		Hint: "作为主网卡与之后新增网口的默认型号。",
	},
	{
		// 静态地址在创建时就给定：建好再改要走「静态地址变更」任务，而那
		// 条路要求先关机——对"开箱就要在某个地址上"的场景不划算。
		Key: "static_ip", Label: "静态地址", Kind: EditKindText, Group: EditGroupNetwork,
		RequiresNode: true, InCreate: true,
		Hint: "留空由 DHCP 分配。必须是节点内网段内的合法地址，否则主网口拿不到地址。",
	},

	// --- 启动与安全 ---
	{
		Key: "os_type", Label: "操作系统类型", Kind: EditKindSelect, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "linux",
		Options: []EditOption{
			{Value: "linux", Label: "Linux"},
			{Value: "windows", Label: "Windows"},
			{Value: "other", Label: "其它"},
		},
		Hint: "影响虚拟化层对硬件的呈现方式。",
	},
	{
		// 系统版本（libosinfo 的 short id）。
		//
		// 它的作用不是"显示更好看"：虚拟化层据此优化设备呈现与驱动建议，
		// 而装系统时的默认磁盘控制器也跟着它走。选错的表现通常是装完起不来，
		// 而那时没人会想到是这一步选错了。
		//
		// 可选值内置而不是每次向节点问：libosinfo 的列表在节点上可能装了
		// 也可能没装，而一个空的下拉框与"这台机器没有可选版本"在界面上
		// 看不出区别。
		Key: "os_variant", Label: "系统版本", Kind: EditKindSelect, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "",
		Options: []EditOption{
			{Value: "", Label: "不指定"},
			{Value: "ubuntu24.04", Label: "Ubuntu 24.04"},
			{Value: "ubuntu22.04", Label: "Ubuntu 22.04"},
			{Value: "debian12", Label: "Debian 12"},
			{Value: "debian11", Label: "Debian 11"},
			{Value: "rocky9", Label: "Rocky Linux 9"},
			{Value: "almalinux9", Label: "AlmaLinux 9"},
			{Value: "centos7", Label: "CentOS 7"},
			{Value: "opensuse15", Label: "openSUSE 15"},
			{Value: "fedora40", Label: "Fedora 40"},
			{Value: "archlinux", Label: "Arch Linux"},
			{Value: "win11", Label: "Windows 11"},
			{Value: "win10", Label: "Windows 10"},
			{Value: "win2k22", Label: "Windows Server 2022"},
			{Value: "win2k19", Label: "Windows Server 2019"},
		},
		Hint: "选具体版本能让虚拟化层给出更合适的默认硬件；不确定时选「不指定」。",
	},
	{
		// 主机名与初始凭据在创建时给：它们是"这台机器第一次开机就该是
		// 什么样"的一部分，建好再设要走来宾自动化，而那要求 Guest Agent
		// 已经在跑——对一个刚装好的系统不成立。
		Key: "hostname", Label: "主机名", Kind: EditKindText, Group: EditGroupBoot,
		RequiresNode: true, InCreate: true,
		Hint: "留空则使用虚拟机名称。",
	},
	{
		Key: "initial_password", Label: "初始登录密码", Kind: EditKindText, Group: EditGroupBoot,
		RequiresNode: true, InCreate: true, CreateOnly: true,
		Hint: "写入来宾凭据记录，供「来宾自动化」与详情页展示用。**只写不读**，设置后只能重设。",
	},
	{
		Key: "machine_type", Label: "机器类型", Kind: EditKindSelect, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "q35",
		Options: []EditOption{
			{Value: "q35", Label: "q35（较新，支持 PCIe）"},
			{Value: "i440fx", Label: "i440fx（兼容老旧系统）"},
		},
	},
	{
		Key: "firmware", Label: "固件类型", Kind: EditKindSelect, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "bios",
		Options: []EditOption{
			{Value: "bios", Label: "BIOS（传统）"},
			{Value: "uefi", Label: "UEFI"},
		},
		Hint: "从 BIOS 改为 UEFI 后，原有系统可能无法引导。",
	},
	{
		Key: "secure_boot", Label: "安全启动", Kind: EditKindBoolean, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "false",
		Hint: "仅在固件为 UEFI 时有效。",
	},
	{
		Key: "boot_order", Label: "引导顺序", Kind: EditKindText, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true,
		Default: "disk,cdrom,network",
		Hint:    "逗号分隔，如 disk,cdrom,network。装系统时把 cdrom 放在最前。",
	},
	{
		Key: "auto_start", Label: "随宿主机自启", Kind: EditKindBoolean, Group: EditGroupBoot,
		RequiresNode: true, InCreate: true, Default: "false",
		Hint: "宿主机重启后自动启动这台虚拟机。",
	},
	{
		Key: "watchdog", Label: "看门狗", Kind: EditKindSelect, Group: EditGroupBoot,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "none",
		Options: []EditOption{
			{Value: "none", Label: "关闭"},
			{Value: "reset", Label: "重启虚拟机"},
			{Value: "poweroff", Label: "强制断电"},
			{Value: "shutdown", Label: "优雅关机"},
		},
		Hint: "来宾无响应时由虚拟化层采取的动作。",
	},

	// --- 高级设置 ---
	{
		Key: "cpu_type", Label: "CPU 型号", Kind: EditKindSelect, Group: EditGroupAdvanced,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "host",
		Options: []EditOption{
			{Value: "host", Label: "host（透传宿主机特性，性能最好）"},
			{Value: "qemu64", Label: "qemu64（通用，便于迁移）"},
		},
	},
	{
		// CPU 亲和性（绑核）。它决定 vCPU 实际跑在哪些物理核上，与"给多少核"
		// 是同一类决策，因此放在同一处。
		Key: "cpu_affinity", Label: "CPU 亲和性（绑核）", Kind: EditKindText, Group: EditGroupAdvanced,
		RequiresNode: true, InCreate: true,
		Hint: "形如 0-3 或 0,2,4。留空表示不绑核，由调度器自行选择物理核。",
	},
	{
		Key: "cpu_limit_percent", Label: "CPU 使用率上限（%）", Kind: EditKindNumber,
		Group: EditGroupAdvanced, InCreate: true, Default: "0",
		RequiresNode: true, Min: 0, Max: 100,
		Hint: "0 表示不限制。",
	},
	{
		Key: "memory_hugepages", Label: "使用内存大页", Kind: EditKindBoolean,
		Group:        EditGroupAdvanced,
		RequiresNode: true, RequiresShutdown: true, Default: "false",
		Hint: "需要宿主机预先分配大页内存。",
	},
	{
		Key: "apic", Label: "APIC", Kind: EditKindBoolean, Group: EditGroupAdvanced,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "true",
		Hint: "老系统可能需要关闭。",
	},
	{
		Key: "pae", Label: "PAE", Kind: EditKindBoolean, Group: EditGroupAdvanced,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "true",
		Hint: "老系统可能需要关闭。",
	},
	{
		Key: "init_mode", Label: "首次启动初始化", Kind: EditKindSelect, Group: EditGroupAdvanced,
		RequiresNode: true, RequiresShutdown: true, InCreate: true, Default: "none",
		Options: []EditOption{
			{Value: "none", Label: "不初始化"},
			{Value: "nocloud", Label: "NoCloud（Linux）"},
			{Value: "configdrive", Label: "ConfigDrive（Windows）"},
			{Value: "openwrt", Label: "OpenWrt"},
		},
		Hint: "仅在首次启动时生效，对已初始化的系统无影响。",
	},
	{
		Key: "freeze_on_start", Label: "启动时冻结", Kind: EditKindBoolean,
		Group: EditGroupAdvanced, InCreate: true, Default: "false",
		RequiresNode: true,
		Hint:         "启动后立即暂停，需手动恢复。用于调试。",
	},
	{
		Key: "guest_agent", Label: "Guest Agent", Kind: EditKindBoolean,
		Group: EditGroupAdvanced,
		// **只读**：它是 agent 上报的探测结果，不是用户能设定的东西。
		// 做成可编辑的输入框会让人以为「勾上它就能让 Guest Agent 跑起来」。
		ReadOnly: true,
		Hint:     "由节点上报。它是「来宾自动化」各项能力的前置条件。",
	},
}

// EditForm 是编辑页的表单元数据与当前值。
type EditForm struct {
	// Fields 是**全部**可编辑项（含那些需要关机才能改的）。
	//
	// 不按运行态过滤：用户需要看到「有哪些项可以改」，只是暂时改不了。
	// 直接隐藏会让他在关机之后再进来才发现多出几项，从而怀疑自己记错了。
	Fields []EditField `json:"fields"`
	// Values 是各项的当前值。
	Values map[string]any `json:"values"`
	// EditableNow 报告**以当前运行态**能否提交需要下发的修改。
	EditableNow bool `json:"editable_now"`
	// CurrentStatus 是做出上述判断所依据的状态。
	CurrentStatus string `json:"current_status"`
	// Groups 是子选项卡的顺序与名称。由后端下发而不是前端硬编码：
	// 新增一个子选项卡时只改这里一处。
	Groups []EditGroupInfo `json:"groups"`
}

// EditGroupInfo 描述一个子选项卡。
type EditGroupInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Planned 表示该子选项卡的内容尚未实现。界面据此显示说明而不是空表格。
	Planned bool `json:"planned"`
	// Note 是未实现时的说明（对应的需求编号与阻塞点）。
	Note string `json:"note,omitempty"`
}

// editGroups 是子选项卡的顺序与名称。
var editGroups = []EditGroupInfo{
	{Key: EditGroupBasic, Label: "基础配置"},
	{Key: EditGroupHardware, Label: "硬件规格"},
	{Key: EditGroupDisk, Label: "磁盘与驱动器"},
	{Key: EditGroupBoot, Label: "启动与安全"},
	{
		Key: EditGroupNetwork, Label: "网口", Planned: true,
		Note: "F-2-05 的网口配置与详情页「网络管理」标签页是同一批数据，" +
			"编辑能力仍在实现中。当前可在「网络管理」中查看。",
	},
	{
		Key: EditGroupPassthru, Label: "硬件直通", Planned: true,
		Note: "PCI 直通需要独立的设备表与宿主机的 IOMMU 分组信息（F-2-06），尚未建模。",
	},
	{Key: EditGroupAdvanced, Label: "高级设置"},
}

// EditFormOf 返回某台虚拟机的编辑表单元数据与当前值。
//
// **实时探测**运行态（而不是读投影）：EditableNow 直接决定界面能否提交，
// 用陈旧的投影可能导致用户解除了按钮禁用、提交后才被拒绝。
func (s *Service) EditFormOf(
	ctx context.Context, vmID int64, v authz.Viewer,
) (*EditForm, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	return &EditForm{
		Fields:        editFields,
		Values:        editValues(vm),
		EditableNow:   current == model.VMStatusStopped,
		CurrentStatus: current,
		Groups:        editGroups,
	}, nil
}

// editValues 取各项当前值。
//
// 按矩阵逐项读取，而不是手写一张 map：漏掉一项的表现是界面上那个输入框
// 永远空白，而用户在保存时会被「提交了空值」的校验挡住——排查起来会先怀疑
// 前端绑定，实际上问题在这里。
func editValues(vm *model.VM) map[string]any {
	out := make(map[string]any, len(editFields))
	rv := reflect.ValueOf(*vm)

	for _, f := range editFields {
		field := rv.FieldByName(dbFieldName(f.Key))
		if !field.IsValid() {
			// 矩阵里登记了但模型里没有对应字段——这是编码错误，
			// 直接暴露而不是静默给一个空值。
			log.Printf("[vm] 矩阵字段 %s 在模型中没有对应项", f.Key)
			out[f.Key] = nil
			continue
		}
		out[f.Key] = field.Interface()
	}
	return out
}

// dbFieldName 把矩阵里的列名转回 Go 字段名（如 os_type → OSType）。
//
// 维护一张显式的对照表，而不是靠命名规则反推：规则反推在遇到 `os_type` →
// `OSType` 这类不规则映射时会猜错，而猜错的代价是那个字段永远读不出来。
func dbFieldName(key string) string {
	if name, ok := columnToField[key]; ok {
		return name
	}
	return key
}

// columnToField 是「矩阵键 → 模型字段名」的对照表。
var columnToField = map[string]string{
	"vcpu":              "VCPU",
	"memory_mb":         "MemoryMB",
	"remark":            "Remark",
	"group_name":        "GroupName",
	"disk_gb":           "DiskGB",
	"disk_format":       "DiskFormat",
	"disk_bus":          "DiskBus",
	"nic_model":         "NicModel",
	"disk_iops_total":   "DiskIOPSTotal",
	"disk_iops_read":    "DiskIOPSRead",
	"disk_iops_write":   "DiskIOPSWrite",
	"disk_bytes_total":  "DiskBytesTotal",
	"disk_bytes_read":   "DiskBytesRead",
	"disk_bytes_write":  "DiskBytesWrite",
	"os_type":           "OSType",
	"os_variant":        "OSVariant",
	"machine_type":      "MachineType",
	"firmware":          "Firmware",
	"secure_boot":       "SecureBoot",
	"boot_order":        "BootOrder",
	"auto_start":        "AutoStart",
	"watchdog":          "Watchdog",
	"cpu_type":          "CPUType",
	"cpu_limit_percent": "CPULimitPercent",
	"cpu_affinity":      "CPUAffinity",
	"memory_hugepages":  "MemoryHugepages",
	"apic":              "APIC",
	"pae":               "PAE",
	"init_mode":         "InitMode",
	"freeze_on_start":   "FreezeOnStart",
	"guest_agent":       "GuestAgent",
}

// fieldToColumn 是上表的反向映射。
var fieldToColumn = func() map[string]string {
	out := make(map[string]string, len(columnToField))
	for col, field := range columnToField {
		out[field] = col
	}
	return out
}()

// UpdateMetadataRequest 是元数据修改请求。
//
// 用指针表示「本次是否提交了这一项」：只有提交的项才会被写入。用值类型会让
// 「清空备注」与「不修改备注」无法区分——前者是用户明确意图，后者是没碰过
// 那个输入框。
type UpdateMetadataRequest struct {
	Remark    *string
	GroupName *string
}

// UpdateMetadata 直接更新控制面元数据（备注、分组）。
//
// **不入队、不探测、不校验运行态**：这两项只存在于我们的数据库里，虚拟化层
// 根本不知道它们，因此没有「运行中不能改」这回事。让它们和硬件配置走同一条
// 路径，会让改个备注也要等一次节点往返。
func (s *Service) UpdateMetadata(
	ctx context.Context, vmID int64, req UpdateMetadataRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	before := map[string]any{}

	if req.Remark != nil {
		updates["remark"] = optStr(strings.TrimSpace(*req.Remark))
		before["remark"] = derefStr(vm.Remark)
	}
	if req.GroupName != nil {
		updates["group_name"] = optStr(strings.TrimSpace(*req.GroupName))
		before["group_name"] = derefStr(vm.GroupName)
	}

	if len(updates) == 0 {
		return nil, api.InvalidParameter("没有需要修改的内容")
	}

	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vm.ID).Updates(updates).Error; err != nil {
		log.Printf("[vm] 更新元数据失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:      "vm.metadata.update",
		Params:      updates,
		BeforeState: before,
		AfterState:  updates,
		Success:     true, ClientIP: clientIP,
	})

	updated, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	lock, err := s.LockOf(ctx, vmID)
	if err != nil {
		return nil, err
	}
	view := toView(updated, lock, time.Now(), s.staleThreshold())
	return &view, nil
}

// ConfigChangeRequest 是一次配置变更请求。
//
// 用 map 而不是逐个字段：矩阵里有多少项，接口就要支持多少项，而逐个字段会
// 让「新增一个可编辑项」变成三处修改（矩阵、请求结构、校验逻辑）——
// 漏掉任何一处都会让那一项在界面上可改、提交后却被静默丢弃。
type ConfigChangeRequest struct {
	Changes map[string]any
}

// UpdateConfig 受理一次配置变更。
//
// 校验链：**字段必须在矩阵里** → **类型必须与矩阵一致** → **范围合法** →
// **需要关机的项要求已关机**。全部通过后入队，由任务队列按资源锁串行
// （与电源操作共用 vm:<id>，因此不会出现「边改配置边开机」）。
func (s *Service) UpdateConfig(
	ctx context.Context, vmID int64, req ConfigChangeRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 只收集**真正变化**的字段（差异提交，F-2-05），并逐项做类型与范围校验。
	//
	// 这与界面上的「只提交变化字段」是同一件事的两端：界面负责不把没动过的
	// 输入框发上来，这里负责再筛一遍。两端都做不是冗余——用户可能改了又改
	// 回来，而「等值修改」在需要重启的项上会白白触发一次重启。
	existing := editValues(vm)
	changes := map[string]any{}

	for key, raw := range req.Changes {
		f, ok := editFieldByKey(key)
		if !ok {
			return nil, api.InvalidParameter("不支持的配置项: " + key)
		}
		if f.ReadOnly {
			return nil, api.InvalidParameter(f.Label + "是由节点上报的状态，不能修改")
		}
		// 只在创建时出现的项（如系统盘容量）**不接受在编辑里改**：改容量
		// 走扩容任务（f-2-06），在那条路径上有「只能扩不能缩」与配额的
		// 校验。在这里放行会绕过它们。
		if f.CreateOnly {
			return nil, api.InvalidParameter(
				f.Label + "只能在创建时设定，之后请通过扩容调整")
		}

		value, err := coerceField(f, raw)
		if err != nil {
			return nil, err
		}
		if sameValue(value, existing[key]) {
			continue
		}
		changes[key] = value
	}

	if len(changes) == 0 {
		return nil, api.InvalidParameter("没有需要修改的内容")
	}

	// IOPS 的「总量」与「读写分离」互斥（f-2-06）。
	//
	// 校验的是**变更后的最终状态**而不是本次提交的字段：用户可能这次只改总量，
	// 而上一次已经设了读写分离——只检查本次提交会漏掉这种组合。
	if err := checkIOPSExclusive(existing, changes); err != nil {
		return nil, err
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	// 需要关机的字段在运行态下一律拒绝。
	//
	// 拒绝而不是「自动关机再改」：自动关机会中断用户正在跑的业务，而那是
	// 一个他没有要求的操作。让他自己决定什么时候停机。
	if current != model.VMStatusStopped && requiresShutdown(changes) {
		return nil, api.ValidationFailed(
			"这些配置需要先关机才能修改（当前为" + DescribeStatus(current) + "）")
	}

	params := configChangeParams{
		VMID: vm.ID, VMName: vm.Name,
		Changes:        changes,
		ObservedStatus: current,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMConfigUpdate,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:      "vm.config.update.request",
		Params:      params,
		BeforeState: existing,
		AfterState:  map[string]any{"changes": changes, "task_id": t.ID},
		Success:     true, ClientIP: clientIP,
	})
	return t, nil
}

// editFieldByKey 按 key 查矩阵。
func editFieldByKey(key string) (EditField, bool) {
	for _, f := range editFields {
		if f.Key == key {
			return f, true
		}
	}
	return EditField{}, false
}

// coerceField 把请求里的原始值转成模型需要的类型。
//
// JSON 经过反序列化后数字是 float64、布尔是 bool、字符串是 string，而前端
// 的输入框统一提交字符串。这里按矩阵声明的 Kind 做转换，而不是靠 Go 的类型
// 断言去猜——猜错的表现是「提交了但没变化」，用户会以为保存失败了。
func coerceField(f EditField, raw any) (any, error) {
	switch f.Kind {
	case EditKindNumber:
		n, err := toInt(raw)
		if err != nil {
			return nil, api.InvalidParameter(f.Label + "必须是整数")
		}
		if f.Min > 0 && n < f.Min {
			return nil, api.InvalidParameter(f.Label + "不能小于 " + strconv.Itoa(f.Min))
		}
		if f.Max > 0 && n > f.Max {
			return nil, api.InvalidParameter(f.Label + "不能大于 " + strconv.Itoa(f.Max))
		}
		return n, nil

	case EditKindBoolean:
		b, err := toBool(raw)
		if err != nil {
			return nil, api.InvalidParameter(f.Label + "必须是布尔值")
		}
		return b, nil

	case EditKindSelect:
		s, ok := toString(raw)
		if !ok {
			return nil, api.InvalidParameter(f.Label + "取值不合法")
		}
		for _, opt := range f.Options {
			if opt.Value == s {
				return s, nil
			}
		}
		// 枚举值必须落在矩阵声明的范围内：放行未知值会让它一路传到节点，
		// 而节点报的错通常是一句看不懂的参数错误。
		return nil, api.InvalidParameter(f.Label + "取值不合法")

	default: // EditKindText
		s, ok := toString(raw)
		if !ok {
			return nil, api.InvalidParameter(f.Label + "必须是文本")
		}
		return strings.TrimSpace(s), nil
	}
}

func toInt(raw any) (int, error) {
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, errNotInteger
		}
		return int(v), nil
	case int:
		return v, nil
	case int64:
		return int(v), nil
	case string:
		trimmed := strings.TrimSpace(v)
		// 空字符串按 0 处理：输入框被清空时前端提交的是空串，而用户的意思是
		// 「不限制」而不是「设为 0 之外的什么东西」。这个约定与矩阵里
		// 「0 表示不限制」的说明一致。
		if trimmed == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(trimmed)
		if err != nil {
			return 0, errNotInteger
		}
		return n, nil
	default:
		return 0, errNotInteger
	}
}

func toBool(raw any) (bool, error) {
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "on", "yes":
			return true, nil
		case "false", "0", "off", "no", "":
			return false, nil
		}
		return false, errNotBool
	case float64:
		return v != 0, nil
	default:
		return false, errNotBool
	}
}

func toString(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		return v, true
	case nil:
		return "", true
	default:
		return "", false
	}
}

// sameValue 比较两个值是否相等。
//
// 跨过一层字符串与数字的转换：界面上的输入框提交的是字符串 "8"，而库里存的
// 是 int 8——直接比较会认为「都变了」，于是把没动过的字段也提交一次。
func sameValue(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if as, ok := toString(a); ok {
		bs, _ := toString(b)
		// 数字与布尔统一转成字符串比较
		return as == stringify(b) || as == bs
	}
	if ab, ok := a.(bool); ok {
		bb, err := toBool(b)
		return err == nil && ab == bb
	}
	if an, err := toInt(a); err == nil {
		bn, err := toInt(b)
		return err == nil && an == bn
	}
	return reflect.DeepEqual(a, b)
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// checkIOPSExclusive 校验磁盘限速的互斥关系（f-2-06）。
//
// 两组规则各管一半：IOPS 与吞吐**可以并存**（它们限制的是不同性质的负载），
// 但每组内部的「总量」与「读写分离」互斥——同时给总量和读、写两套上限的
// 话，生效的是哪一套取决于节点的实现，而用户无法从界面上判断。
func checkIOPSExclusive(existing map[string]any, changes map[string]any) error {
	after := func(key string) int {
		if v, ok := changes[key]; ok {
			if n, ok := v.(int); ok {
				return n
			}
		}
		if n, err := toInt(existing[key]); err == nil {
			return n
		}
		return 0
	}

	total := after("disk_iops_total")
	read := after("disk_iops_read")
	write := after("disk_iops_write")
	if total > 0 && (read > 0 || write > 0) {
		return api.ValidationFailed(
			"IOPS 限制的「总量」与「读写分离」互斥，请只设置其中一组")
	}

	bytesTotal := after("disk_bytes_total")
	bytesRead := after("disk_bytes_read")
	bytesWrite := after("disk_bytes_write")
	if bytesTotal > 0 && (bytesRead > 0 || bytesWrite > 0) {
		return api.ValidationFailed(
			"吞吐限制的「总量」与「读写分离」互斥，请只设置其中一组")
	}
	return nil
}

// requiresShutdown 报告变更集合中是否有需要关机的字段。
func requiresShutdown(changes map[string]any) bool {
	for key := range changes {
		if f, ok := editFieldByKey(key); ok && f.RequiresShutdown {
			return true
		}
	}
	return false
}

// configChangeParams 是 vm.config.update 任务的参数。
type configChangeParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// Changes 是**仅包含变化项**的字段集合（差异提交）。
	Changes map[string]any `json:"changes"`
	// ObservedStatus 是受理时探测到的状态，仅作排障线索。
	ObservedStatus string `json:"observed_status"`
}

// FieldNameOf 返回矩阵键对应的模型字段名，供执行器回写时使用。
func FieldNameOf(key string) string { return dbFieldName(key) }

// ColumnOfField 返回 Go 字段名对应的列名。
func ColumnOfField(field string) string { return fieldToColumn[field] }
