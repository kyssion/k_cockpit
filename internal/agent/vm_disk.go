package agent

// OpVMDiskList 读取一台虚拟机的磁盘列表。
//
// 它是**只读探测**，与 OpVMStats / OpVMStatus 同类：不入队、不写投影，
// 只在被请求时向节点取一次。
//
// 为什么不把磁盘存进控制面的表：磁盘是**虚拟化层的状态**，控制面存一份
// 就要承担与它对账的责任——多一块少一块都不会报错，只会在某次扩容或快照
// 时表现为"改了一块不存在的盘"。列表页读缓存是另一回事（那是量大的高频
// 读），而磁盘列表只在打开详情页的一个标签时读一次。
const OpVMDiskList OpKind = "vm.disk.list"

// OpVMDiskChange 变更一块磁盘：挂载 / 卸载 / 换总线。
//
// 三个动作共用一个 Kind，理由与 OpVMCDROMApply 相同：对节点而言它们都是
// "把这台虚拟机的磁盘配置改成这个样子"，差别只在参数。拆成三个 Kind 会让
// 节点侧把同一段设备重建逻辑写三遍，而那正是最容易分叉的地方。
//
// **IOPS 限值不在这里**：它走 vm.config.update（与其它配置项同一条路），
// 单独再开一条会让"改 IOPS"有两套实现——而两套实现的校验迟早不一样。
const OpVMDiskChange OpKind = "vm.disk.change"

// VMDiskListDataKey 是列表结果中承载数据的键。
const VMDiskListDataKey = "vm_disks"

// 磁盘变更的动作。取值与 OpVMDiskChange 的 `action` 参数一致。
const (
	DiskActionAttach = "attach"
	DiskActionDetach = "detach"
	DiskActionBus    = "bus"
	// DiskActionMigrate 把一块磁盘**搬到其他存储池**。
	//
	// 它与"复制一份再挂上"的区别是目的：迁移是为了腾空间或换介质，源只有
	// 一份，搬完旧的就没了。因此参数里带的是**目标池**，而不是一个文件
	// 路径——池在哪、还剩多少空间，只有节点知道。
	DiskActionMigrate = "migrate"
)

// 删除虚拟机时的磁盘处理方式。取值与 OpVMDelete 的 `disk_action` 参数一致。
const (
	// DiskActionKeep 保留磁盘文件（默认之外的显式选择）。
	DiskActionKeep = "keep"
	// DiskActionDelete 连同磁盘文件一起删除。
	DiskActionDelete = "delete"
	// DiskActionTransfer 把磁盘文件搬回「我的存储 - 虚拟磁盘」。
	//
	// 它存在的理由很具体：用户删机器时常常是想留着数据的，而"保留"只是
	// 把文件留在原地不删——那块盘会一直在宿主机上占着空间，却不属于任何
	// 虚拟机，谁也看不见它。转移让这份数据回到用户自己的文件列表里。
	DiskActionTransfer = "transfer"
)

// VMDiskTransferDataKey 是删除时「转移到我的存储」的回传数据键。
const VMDiskTransferDataKey = "transferred_disks"

// TransferredDisk 描述一个被转移回「我的存储」的磁盘文件。
//
// 由节点回传而不是控制面自己算：文件搬到了哪个目录、叫什么名字、实际多大，
// 这三件事只有做搬运动作的一方知道。控制面凭设备名猜出来的路径，与真实
// 文件差一个字符就会变成"我的存储里有一个点不开的文件"。
type TransferredDisk struct {
	Dev       string
	Filename  string
	RelPath   string
	SizeBytes int64
}

// VMDisk 是虚拟机上的一块磁盘。
type VMDisk struct {
	// Dev 是来宾里看到的设备名（vda / vdb / sda…）。
	//
	// **它是这块盘的唯一标识**，而不是数据库 id：节点侧按设备名定位，而
	// 控制面并不持有这些盘的记录。用序号或路径当标识在热插拔之后都会漂。
	Dev string
	// CapacityGB 是**配置容量**；ActualBytes 是宿主机上的实际占用。
	//
	// 两个都要：qcow2 是稀疏文件，一台配 500 GB 的机器可能只占 20 GB。只
	// 给一个数字的话，用户要么以为盘快满了，要么以为配额算错了——而配额
	// 恰恰是按配置容量算的（f-9-02）。
	CapacityGB  int
	ActualBytes int64
	Format      string
	Bus         string
	// Source 是宿主机上的镜像路径，**仅用于展示与排障**；控制面不解释它。
	Source string
	// IsSystem 标记系统盘：它不可卸载。
	IsSystem bool
	// Hotpluggable 表示**当前**能否热插拔，由节点按机型与空闲槽位判断。
	//
	// 必须由节点给而不是控制面猜：q35 需要空闲的 PCIe 根端口，槽位耗尽时
	// 节点会降级到 virtio-scsi——这类判断只有虚拟化层做得出，控制面猜错的
	// 结果是用户点了一个注定失败的按钮。
	Hotpluggable bool
}

// VMDiskChangeInfo 是变更结果。
type VMDiskChangeInfo struct {
	// Dev 是变更后的设备名。挂载时由节点分配，控制面无法预知——
	// 少了一个序号还是被降级到另一条总线，只有节点知道。
	Dev string
	// RebootNeeded 为 true 表示这次改动要重启虚拟机才生效。
	//
	// 换总线几乎一定需要（来宾里的驱动与设备路径都变了）。与光驱那条同理：
	// "重启整台机器"与"来宾里刷新一下"代价差一个量级，必须分开说。
	RebootNeeded bool
	// GuestRefreshNeeded 为 true 表示来宾里可能需要重新扫描或挂载。
	GuestRefreshNeeded bool
	Message            string
}
