package agent

// OpOVSStatus 读取 Open vSwitch 的运行状态。**只读**。
//
// 它与 OpNetworkProbe 的分工：后者回答「有没有装、能不能用」，而这一份回答
// 「装的那份现在健康吗」——服务在不在跑、OpenFlow 版本对不对、meter 表是否
// 可用、有多少端口与流表。
//
// 分开是必要的：OVS 装好了但服务挂了，探测说「可用」，而端口安全与镜像
// 全部失效——用户看到的是一个"能力齐全"的界面和一堆不生效的规则。
const OpOVSStatus OpKind = "ovs.status"

// OpOVSPorts 列出 OVS 上的端口。**只读**。
//
// 端口列表是排查「虚拟机的网口到底挂上了没有」的最直接入口：控制面知道
// 该挂哪个，而这个列表说明实际挂了哪个。两者不一致时，用户在虚拟机里
// 看不到网络，而面板上一切正常。
const OpOVSPorts OpKind = "ovs.ports"

// OpDHCPLeases 读取 DHCP 租约。**只读**。
//
// 它回答两个具体问题：某个 IP 现在分配给了谁（**排查 IP 冲突**的第一步），
// 以及为什么某台机器没拿到预期的地址（租约被别的主机占了）。
const OpDHCPLeases OpKind = "dhcp.leases"

// OpPlatformCheck 按**期望状态**逐项比对节点上的实际状态。**只读**。
//
// 这是自检与探测的根本区别：
//
//	探测回答「这台机器有没有 OVS」——它看的是环境。
//	自检回答「我们配的那些东西现在还在不在」——它看的是**偏差**。
//
// 后者才是用户真正需要的东西：面板上显示「端口安全已启用」而节点上的流表
// 早就被一次重启清掉了，这个状态不会以任何形式报警。自检正是去找它。
//
// 因此它接收控制面的期望状态（端口安全策略、公网 IP 绑定、端口镜像、VPC
// 交换机），逐个问节点「这个现在实际是什么样」。
const OpPlatformCheck OpKind = "platform.check"

// OpPlatformRepair 按期望状态重新下发。
//
// 它**不是"一键变好"**：修复的能力受限于节点——缺 OVS 装不上、缺内核模块
// 也加载不了。因此参数里带上要修的具体项，而结果里逐项说明成功与失败，
// 而不是笼统地回一个"已修复"。
const OpPlatformRepair OpKind = "platform.repair"

// OVSStatusKey 是 OVS 状态的键。
const OVSStatusKey = "ovs_status"

// OVSPortsKey 是端口列表的键。
const OVSPortsKey = "ovs_ports"

// DHCPLeasesKey 是租约列表的键。
const DHCPLeasesKey = "dhcp_leases"

// PlatformCheckKey 是自检结果的键。
const PlatformCheckKey = "platform_check"

// OVSStatus 是 Open vSwitch 的运行状态。
type OVSStatus struct {
	// Available 为 false 时 Reason 说明原因。
	Available bool
	Reason    string
	// Version 是 OVS 版本。
	Version string
	// ServiceActive 表示 ovs-vswitchd 服务在跑。
	//
	// **与 Available 分开**：装好了但服务挂了是最容易被忽略的一种状态——
	// 探测说"可用"，而所有依赖它的功能都不生效。
	ServiceActive bool
	// OpenFlow13 表示支持 OpenFlow 1.3（端口安全与镜像依赖它）。
	OpenFlow13 bool
	// MeterAvailable 表示 meter 表可用（包速率限制依赖它）。
	MeterAvailable bool
	// BridgeCount / PortCount / FlowCount 是规模概览。
	BridgeCount int
	PortCount   int
	FlowCount   int
	// Fix 给出不可用时要做什么。
	Fix string
}

// OVSPort 是 OVS 上的一个端口。
type OVSPort struct {
	Name   string
	Bridge string
	// Type 是端口类型（internal / system / vnet）。
	Type string
	// Tag 是 VLAN tag（0 表示未打标）。
	Tag int
	// VMName 是该端口对应的虚拟机（控制面填），空表示不是虚拟机的口。
	//
	// 节点只能看到 vnet 这样的口名，而"这个口属于哪台机器"要靠控制面
	// 对上——没有它，用户在一列 vnet0/vnet1 里找不出自己要找的那台。
	VMName string
}

// DHCPLease 是一条租约。
type DHCPLease struct {
	// ExpiresAt 是到期时刻（RFC3339，空表示永不过期）。
	ExpiresAt string
	MAC       string
	IP        string
	Hostname  string
	// ClientID 是客户端标识，排查"同一个 MAC 拿了两个地址"时要用。
	ClientID string
}

// PlatformCheck 是一次自检的结果。
type PlatformCheck struct {
	// Items 是逐项检查结果。
	Items []CheckItem
	// Drifts 是**偏差数量**（配置与实际不一致的项数）。
	//
	// 它是自检最核心的一个数字：环境缺什么是"装没装"的问题，而偏差是
	// "我们以为它在，其实它不在"——后者不报警、不被发现，直到用户自己撞上。
	Drifts int
}

// CheckItem 是一项检查结果。
type CheckItem struct {
	// Category 是分类（网络底座 / 端口安全 / 公网地址 / 端口镜像 ...）。
	Category string
	// Target 是被检查的具体对象。
	Target string
	// Expected 是控制面的期望状态。
	Expected string
	// Actual 是节点上的实际状态。
	Actual string
	// OK 为 false 表示这是一处**偏差**。
	OK bool
	// Severity 取 info / warning / critical。
	//
	// 分级而不是一律"有问题"：一个不影响连通性的偏差（如少了一条计数规则）
	// 与"整个 VPC 隔离失效"不该得到同样的注意力。
	Severity string
	// Fix 说明这一项要做什么。
	Fix string
	// Repairable 表示能不能通过重新下发修复。
	//
	// **不能修的要说出来**：缺 OVS 装不上、内核模块加载不了——把它们混在
	// "可修复"里，用户会点那个按钮、等一会、然后发现什么都没变。
	Repairable bool
}

// PlatformRepairInfo 是修复结果。
type PlatformRepairInfo struct {
	// Repaired 是修复成功的项数。
	Repaired int
	// Failed 是修复失败的项及其原因。
	Failed  []string
	Message string
}

// PlatformExpectation 是一条**期望状态**，由控制面按自己的记录生成。
//
// 自检的分工是刻意的：**控制面说「应该有什么」，节点说「实际有什么」**。
// 让节点自己判断应该有什么是不可能的——期望状态在控制面的库里，而节点与
// 控制面的数据库之间不该有依赖。
type PlatformExpectation struct {
	// ID 是这条期望对应的控制面记录 id（回传时原样带回，用于对上号）。
	ID int64
	// Kind 是类别：port_security / public_ip / port_mirror。
	Kind string
	// Target 是被检查的对象（网口、绑定 id、镜像名）。
	Target string
}

// PlatformCheckResult 是一条期望的实际状态。
type PlatformCheckResult struct {
	ID int64
	// Present 为 true 表示节点上**确实存在**对应的配置。
	Present bool
	// Actual 是对实际状态的人话描述（「无对应流表」/「已存在 3 条规则」）。
	Actual string
}
