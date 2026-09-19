package agent

// OpVMDiskResize 把虚拟机的磁盘**扩大**到指定容量。
//
// 与 GuestActionExpandDisk 的分工是这个接口存在的理由：
//
//	expand_disk（guest 动作）  运行中 + 来宾里装了 agent 时用。它会**顺带在
//	                          来宾里扩文件系统**，用户拿到的是立刻可用的空间。
//	本操作                      关机时用，或者来宾里没有 agent 时用。它只扩
//	                          宿主机这一侧（镜像文件与域定义），**来宾里的
//	                          分区与文件系统仍要自己扩**。
//
// 少了这一条路径，没有 agent 的用户就只剩手工 `qemu-img resize`——而那样
// 做完之后分区表对不上，最后得到一台「盘大了但用不了」的机器。
const OpVMDiskResize OpKind = "vm.disk.resize"

// VMDiskDataKey 是结果中承载补充信息的键。
const VMDiskDataKey = "vm_disk"

// VMDiskResizeInfo 是扩容结果。
type VMDiskResizeInfo struct {
	// Applied 为 true 表示宿主机这一侧已经扩好。
	Applied bool
	// OldGB / NewGB 是扩容前后的容量。
	OldGB int
	NewGB int
	// GuestGrowNeeded 为 true 表示**来宾里还需要扩分区与文件系统**。
	//
	// 必须显式告诉用户：宿主机侧扩完只是"盘子变大了"，而操作系统看到的
	// 仍然是原来的分区。不提醒的话，用户会以为扩容失败。
	GuestGrowNeeded bool
	// GuestGrowHint 给出具体的做法（依来宾系统不同）。
	GuestGrowHint string
	Message       string
}
