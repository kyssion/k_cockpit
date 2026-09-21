package agent

// 存储池的四项补充能力（F-5-01 的后续迭代）：分区、池配置、卸载、trim。
//
// 它们都是**节点侧的事实**：分区表在磁盘上、挂载与 fstab 在宿主机上、
// trim 是对块设备下发 discard。控制面只能下发动作并展示结果，不该自己
// 推算"会分成哪个区号"或者"挂载点现在是什么"。
const (
	// OpStoragePartitions 列出一块磁盘上的分区。
	OpStoragePartitions OpKind = "storage.partition.list"
	// OpStoragePartitionCreate 在空闲空间上创建分区。
	OpStoragePartitionCreate OpKind = "storage.partition.create"
	// OpStoragePartitionDelete 删除分区（可一次删除全部）。
	OpStoragePartitionDelete OpKind = "storage.partition.delete"
	// OpStoragePoolConfig 下发池配置：挂载点、文件系统、开机自动挂载。
	OpStoragePoolConfig OpKind = "storage.pool.config"
	// OpStoragePoolUnmount 卸载存储池（保留数据，只摘挂载并清 fstab）。
	OpStoragePoolUnmount OpKind = "storage.pool.unmount"
	// OpStorageTrim 对块设备下发 trim / discard。
	OpStorageTrim OpKind = "storage.trim"
)

// PartitionListDataKey 是分区列表的键。
const PartitionListDataKey = "partitions"

// PartitionInfo 是一个分区。
type PartitionInfo struct {
	// Index 是分区号（1 起）。界面显示"分区 3"而不是设备路径，因为用户在
	// fdisk 里看到的就是编号。
	Index int
	// Path 是设备路径（如 /dev/sdb3）。
	Path      string
	SizeBytes int64
	// FSType 为空表示未格式化。
	FSType string
	// Mounted 表示当前是否已挂载。
	Mounted bool
	// System 表示这是系统盘上的分区：删它的后果不是"少一块空间"，而是
	// 宿主机可能起不来。
	System bool
	// InUseByPool 非空表示它已被某个存储池占用（节点侧判断）。
	InUseByPool string
}

// PartitionDataKey 是分区操作结果的键。
const PartitionDataKey = "partition"

// PartitionResult 是分区创建 / 删除的结果。
type PartitionResult struct {
	Index     int
	Path      string
	SizeBytes int64
	// Deleted 是本次删除的分区数（删除全部时大于 1）。
	Deleted int
	Message string
}

// PoolConfigDataKey 是池配置下发结果的键。
const PoolConfigDataKey = "pool_config"

// PoolConfigResult 是池配置的结果。
type PoolConfigResult struct {
	// MountPath 是节点实际生效的挂载点。
	//
	// 回传而不是沿用请求里的值：节点可能按池名规范化了路径。以它为准，
	// 否则面板上的挂载点会从下次刷新开始与真实位置不一致。
	MountPath string
	// AutoMount 是实际生效的开机自动挂载设置。
	AutoMount bool
	Message   string
}

// PoolUnmountDataKey 是卸载结果的键。
const PoolUnmountDataKey = "pool_unmount"

// PoolUnmountResult 是卸载的结果。
type PoolUnmountResult struct {
	Unmounted bool
	// DataKept 表示数据是否保留。为 false 意味着数据被清掉了——那不是
	// 卸载该有的结果，界面必须如实说。
	DataKept bool
	Message  string
}

// TrimDataKey 是 trim 结果的键。
const TrimDataKey = "trim"

// TrimResult 是 trim 的结果。
type TrimResult struct {
	// Devices 是本次处理的设备数。
	Devices int
	// ReclaimedBytes 是回收的字节数（节点上报，可能为 0）。
	ReclaimedBytes int64
	Message        string
}
