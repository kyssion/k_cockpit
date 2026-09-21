package agent

// PCIe 根端口与虚拟机邻居表：这两个都是**排查时才会看**的东西，
// 因此走按需操作而不进周期采样。

// OpVMPcieInfo 读取一台虚拟机的 PCIe 根端口使用情况。
//
// 为什么需要它：热插拔一块盘或一块网卡时，q35 机型需要一个空闲的 PCIe
// 根端口。槽位用完的表现是"挂载命令成功、设备却没出现"——而那个报错里
// 不会提到槽位。i440FX 根本没有根端口，因此它不支持热插拔。
const OpVMPcieInfo OpKind = "vm.pcie.info"

// PCIeDataKey 是 PCIe 信息的键。
const PCIeDataKey = "pcie"

// PCIeInfo 是一台虚拟机的 PCIe 根端口情况。
type PCIeInfo struct {
	// Total 是根端口总数。
	Total int
	// Free 是空闲数量。它是"还能再插几块"的直接答案。
	Free int
	// HotplugSupported 为 false 表示这台机器不支持热插拔（如 i440FX）。
	//
	// 与 Free = 0 分开：槽位用完可以扩，机型不支持则只能换机型——这两种
	// 情况给用户的建议完全不同。
	HotplugSupported bool
	// Reason 说明不支持或读不到的原因。
	Reason string
	// MachineType 回显当前机型，便于界面解释"为什么不支持"。
	MachineType string
}

// OpVMNeighbors 读取一台虚拟机所在二层网络的邻居表（ARP / NDP）。
//
// 它回答的是"这台机器现在能看见谁"。排查"虚拟机之间不通"时，先看邻居表
// 能不能学到对端 MAC，比直接抓包快一个数量级：邻居表里没有对端说明问题在
// 二层（交换机、VLAN、端口隔离），有对端则要看三层（安全组、防火墙）。
const OpVMNeighbors OpKind = "vm.neighbor"

// NeighborDataKey 是邻居表结果的键。
const NeighborDataKey = "neighbors"

// NeighborEntry 是邻居表中的一条。
type NeighborEntry struct {
	IP  string
	MAC string
	// Interface 是学到这条的网口名（如 vnet0）。
	Interface string
	// State 取值 reach / stale / delay / probe / failed / permanent。
	//
	// failed 尤其值得显示：它意味着"有一条 ARP 记录但解析不到 MAC、二层不通。
	State string
	// IsSelf 标记这条是本机自己。
	IsSelf bool
	// VMName 由控制面回填：把 MAC 对应到面板里的机器名，否则用户要自己拿 MAC 去对。
	VMName string
	// Bridge 是所属网桥。
	Bridge string
	// Unavailable 非空表示读不到邻居表（节点未实现）。
	Unavailable string
}
