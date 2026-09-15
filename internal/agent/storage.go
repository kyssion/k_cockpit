package agent

// 存储相关的领域操作。
const (
	// OpNodeDisks 探测节点的块设备清单。**只读**。
	OpNodeDisks OpKind = "node.disks"

	// OpStoragePoolCreate 格式化并挂载设备，建立存储池。
	//
	// 这是**不可逆操作**：格式化会销毁设备上原有的数据。控制面要求用户
	// 输入设备名确认，并要求完成二次验证（f-5-01 R-004）。
	OpStoragePoolCreate OpKind = "storage.pool.create"

	// OpStoragePoolDelete 卸载并删除存储池。
	OpStoragePoolDelete OpKind = "storage.pool.delete"
)

// OpStoragePoolScan 探测存储池内的磁盘清单。**只读**。
//
// 存在的理由是 f-5-01 R-008：删除存储池前必须做占用检查。池内是否有磁盘
// 只有节点清楚（虚拟机磁盘按路径归属，控制面不掌握），因此这里向节点问
// 一次，而不是凭控制面记录猜。
const OpStoragePoolScan OpKind = "storage.pool.scan"

// DiskListKey 是 OpNodeDisks 结果中承载设备清单的键。
const DiskListKey = "disks"

// VolumeListKey 是 OpStoragePoolScan 结果中承载卷标识清单的键。
const VolumeListKey = "volumes"

// Disk 描述一块块设备。
//
// 它是 agent 上报的**探测结果**，不是控制面的持久数据——设备可热插拔，
// 缓存一份清单只会在设备变化时误导用户。
type Disk struct {
	// DeviceID 是**稳定标识**（如 /dev/disk/by-id/...），用于唯一确定一块
	// 设备。设备路径会因插入顺序变化（今天 /dev/sdb、明天 /dev/sdc），
	// 用它做唯一键会导致「明明没动过，却说设备不存在」（f-5-01 R-010）。
	DeviceID string `json:"device_id"`
	// Path 是当前设备路径，**仅供展示**。
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`

	// IsSystem 表示该设备（或其分区）承载根文件系统、/boot 或 swap。
	// 系统盘绝不允许被格式化。
	IsSystem bool `json:"is_system"`
	// Mounted 表示设备或其任一子分区处于挂载状态。
	Mounted bool `json:"mounted"`
	// HasData 表示已存在分区表或可识别的文件系统。
	//
	// 它不阻止操作，但界面上必须明确标注——用户看到的应当是「该盘似乎
	// 包含数据」，而不是一个空白的设备名。
	HasData bool `json:"has_data"`

	Filesystem string `json:"filesystem,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`
}

// Usable 报告该设备是否可以（在通过显式确认后）用于创建存储池。
//
// 只排除**绝不允许**的情况；「已有数据」不算——它需要用户显式确认，
// 而不是直接禁止（否则一块曾被格式化过的盘将永远无法使用）。
func (d Disk) Usable() bool {
	return !d.IsSystem && !d.Mounted
}
