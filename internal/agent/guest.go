package agent

// 来宾自动化（F-2-10）。
//
// 这一类的共同点是**要碰来宾系统内部**：改密码、分区格式化、扩容文件系统。
// 因此执行路径分成两段——
//
//	宿主机阶段：在 QEMU/KVM 层做的事（挂载镜像、附加磁盘、改域配置）
//	来宾阶段：  在来宾系统里做的事（由 Guest Agent 执行命令，或离线时挂载后操作）
//
// 两段的分界必须在**阶段上报**里体现出来：宿主机阶段失败通常是权限、路径、
// 设备占用这类问题；来宾阶段失败通常是系统没起来、agent 没装、密码策略拒绝
// 这类问题。两者的排查方向完全不同，混在一条时间线里会让人找错方向。
//
// 阶段的 key 前缀约定：`host.` 与 `guest.`（见 guestStagePlan）。
const OpVMGuest OpKind = "vm.guest"

// 来宾自动化的动作。
const (
	// GuestActionPasswordOnline 在线改密：经 Guest Agent 在运行的来宾里改。
	//
	// **要求虚拟机运行中**，且来宾里跑着 QEMU Guest Agent。它是最安全的
	// 一种改密方式——不需要挂载磁盘，也不会碰到文件系统的任何元数据。
	GuestActionPasswordOnline = "password_online"

	// GuestActionPasswordOffline 离线改密：把系统盘挂到宿主机上改。
	//
	// 用在来宾起不来、或没装 agent 的时候。**要求关机**——挂载一块正在被
	// 写入的磁盘会同时损坏控制面和来宾两侧看到的内容。
	GuestActionPasswordOffline = "password_offline"

	// GuestActionDiskAttach 附加磁盘并自动分区、格式化、挂载。
	//
	// 需要来宾里跑着 agent（它要进去执行分区命令）。**格式化不可逆**，
	// 因此调用方必须显式确认目标磁盘上没有需要的数据。
	GuestActionDiskAttach = "disk_attach"

	// GuestActionExpandDisk 把系统盘扩大后，进来宾扩容文件系统。
	//
	// 分两段：宿主机侧加长虚拟磁盘（快、可逆），来宾侧扩文件系统（要
	// agent，且某些文件系统不支持在线扩容——那时需要重启后再做）。
	GuestActionExpandDisk = "expand_disk"
)

// GuestDataKey 是来宾自动化结果中承载补充信息的键。
const GuestDataKey = "guest"

// GuestInfo 是节点在来宾自动化完成后回传的信息。
type GuestInfo struct {
	// Message 面向用户的补充说明，例如「新分区已挂载到 /data」。
	Message string
	// GuestAgentUsed 表示本次操作是否真的用到了 Guest Agent。
	//
	// 它值得单独回报：离线改密与在线改密对用户来说都是「改密码」，但前者
	// 绕过了来宾系统，后者没有。事后追查「为什么改完密码后 SELinux 上下文
	// 不对」时，这一条是第一个要看的线索。
	GuestAgentUsed bool
}
