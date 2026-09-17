package agent

// 网络底座相关操作（F-4-01 / F-4-13）。
const (
	// OpNetworkProbe 探测节点上的网络能力。
	//
	// f-4-01 要求「agent 在纳管时自探测网络能力并上报」——但能力不是一次
	// 性的：内核模块可能被卸载、OVS 服务可能停掉、物理口可能被拔。因此
	// 它是一个**可以随时重问**的只读探测，而不是纳管那一刻的快照。
	OpNetworkProbe OpKind = "network.probe"

	// OpNetworkBridgeApply 创建或更新一张桥。
	OpNetworkBridgeApply OpKind = "network.bridge.apply"

	// OpNetworkBridgeDelete 删除一张桥。
	OpNetworkBridgeDelete OpKind = "network.bridge.delete"

	// OpNetworkUplinkAttach 把物理口加入桥。
	//
	// **这是整个网络模块里最危险的一个操作**。把一个物理口加到桥上会重置
	// 它的 IP 配置——如果那是管理口（或者管理流量恰好经过它），操作者
	// **当场失联**，而那时他已经没有任何界面路径可以改回来。
	//
	// 因此它必须：
	//   - 带 watchdogs_seconds，节点侧到点未收到确认就**自动把口摘出来**
	//     （与端口镜像同一套思路：控制面是通过网络下发指令的，而这个操作
	//     的结果恰恰可能是网络断掉——依赖网络的保险在需要它时一定不在）；
	//   - 允许操作者显式确认保持。
	OpNetworkUplinkAttach OpKind = "network.uplink.attach"

	// OpNetworkUplinkConfirm 取消入桥的自动回滚。
	OpNetworkUplinkConfirm OpKind = "network.uplink.confirm"

	// OpNetworkUplinkDetach 把物理口从桥里摘出来（手动或自动回滚）。
	OpNetworkUplinkDetach OpKind = "network.uplink.detach"

	// OpNetworkRepair 尝试把网络恢复到期望状态（F-4-13 的「修复」入口）。
	//
	// 它是一个**幂等的收敛动作**，而不是「重试上一次失败的操作」：重试
	// 一件已经失败的事通常不会得到不同结果，而收敛会先看清现状再补差异。
	OpNetworkRepair OpKind = "network.repair"
)

// NetworkCapabilityKey 是探测结果中承载能力的键。
const NetworkCapabilityKey = "capability"

// NetworkRepairKey 是修复结果中承载补充信息的键。
const NetworkRepairKey = "network_repair"

// NetworkCapability 是节点的网络能力（f-4-01：探测 + 降级）。
type NetworkCapability struct {
	// OVSAvailable 为 false 时控制面必须**降级**到 Linux 网桥。
	OVSAvailable bool
	// OVSVersion 供展示与排障；OVS 不可用时为空。
	OVSVersion string
	// KernelModules 是已加载的相关内核模块。
	KernelModules []string
	// UplinkCandidates 是可用的物理上行口候选。
	//
	// 由节点给出，而不是让用户在界面上手填一个网卡名——填错的后果是
	// 把管理口加进桥里然后失联，而候选列表把「可控的选择」限定在了
	// 真实存在的那些口上。
	UplinkCandidates []UplinkCandidate
	// Notes 是降级时会用到的说明。
	Notes []string
}

// UplinkCandidate 是一个可用的上行口。
type UplinkCandidate struct {
	Name string
	// Up 表示链路是通的。
	Up bool
	// HasIP 表示该口上已有 IP 配置。
	//
	// **这是最重要的一个字段**：有 IP 的口很可能是管理口，把它加进桥里
	// 就是把自己关在门外。界面据此把这类候选标红，并要求额外确认。
	HasIP bool
	// Speed 供展示（如 "1000Mb/s"）。
	Speed string
}

// NetworkRepairInfo 是修复结果。
type NetworkRepairInfo struct {
	// Fixed 是本次修复的问题条目。
	Fixed []string
	// Remaining 是修复之后仍然存在的问题。
	//
	// 与 Fixed 分开是必要的：一次修复部分成功时，只报告「修好了什么」
	// 会让用户以为没事了；而只报告「还有问题」又看不出这次操作做了什么。
	Remaining []string
	Message   string
}
