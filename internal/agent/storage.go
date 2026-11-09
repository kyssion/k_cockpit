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

// OpStoragePoolStat 探测存储池挂载点的容量。**只读**。
//
// 存在的理由是 f-5-01 R-009「容量随指标周期上报」：控制面的容量刷新循环
// 定期把该节点全部池的挂载点**一次性**发过来批量 statfs——每节点一次
// 往返，而不是每池一次。批量语义放在这条操作里，池再多也只有一个往返。
const OpStoragePoolStat OpKind = "storage.pool.stat"

// OpStorageFilePush 接收控制面推送的一份文件内容分块。**只写不提交**：
// 分块写进 `<rel_path>.kcpart` 暂存文件，完整性由 OpStorageFileCommit 收口。
//
// 存在的理由：跨机部署下节点拿不到控制面的本机临时路径——旧实现把控制面
// 拼接文件的本地路径塞进 staged 参数，agent 在节点上 rename 一个不存在的
// 文件必然失败（mock 传输移除后实机暴露）。分块经 Execute 通道走 JSON，
// 2MiB 原始数据 base64 后约 2.8MB，低于 gRPC 默认 4MB 消息上限。
const OpStorageFilePush OpKind = "storage.file.push"

// DiskListKey 是 OpNodeDisks 结果中承载设备清单的键。
const DiskListKey = "disks"

// VolumeListKey 是 OpStoragePoolScan 结果中承载卷标识清单的键。
const VolumeListKey = "volumes"

// PoolStatListKey 是 OpStoragePoolStat 结果中承载容量清单的键。
const PoolStatListKey = "pools"

// PoolStat 是一个存储池挂载点的容量快照（statfs 的对外形状）。
type PoolStat struct {
	// Path 是挂载点路径，控制面按它把结果对回池记录。
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	// FreeBytes 是可用空间（Bavail 口径）：未特权用户真正能写入的量。
	FreeBytes int64 `json:"free_bytes"`
}

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
//
// 零容量设备同样不可用：空闲的 nbd 与 zram 在设备清单里表现为
// size=0 的「整盘」（实机验证），把它们标成 usable 会让用户选中后
// 直到 mkfs 才失败——错误应该在选盘时就被拦下。
func (d Disk) Usable() bool {
	return !d.IsSystem && !d.Mounted && d.SizeBytes > 0
}
