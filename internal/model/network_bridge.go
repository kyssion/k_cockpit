package model

import "time"

// 网络后端（f-4-01：能力探测 + 降级）。
const (
	// BackendBridge 是 Linux 网桥，**基础模式**。
	//
	// 它在任何 Linux 上都存在，因此是可以始终依赖的那一档；代价是
	// 缺少 OVS 的流表、隧道与精细的端口统计。
	BackendBridge = "bridge"
	// BackendOVS 是 Open vSwitch，**增强模式**。
	//
	// 缺 OVS 的节点必须仍能跑起来（ADR-0004），因此它是可选的。
	BackendOVS = "ovs"
)

// 桥的工作模式。
const (
	// BridgeModeNAT 内置 DHCP + NAT：来宾走地址转换出网，最通用。
	BridgeModeNAT = "nat"
	// BridgeModeRouted 路由模式：需要上游有一条回程路由。
	BridgeModeRouted = "routed"
	// BridgeModeIsolated 空交换机：只在内部通，**不接外网**。
	//
	// 端口镜像的目标必须是这一种——镜像出去的流量不该有机会流到外面。
	BridgeModeIsolated = "isolated"
)

// 桥的状态。
const (
	BridgeActive  = "active"
	BridgePending = "pending"
	// BridgeError 表示最近一次操作失败了。
	//
	// 它**不等于桥不能用**：一个桥可能只是 DHCP 没起来，而二层转发仍然
	// 正常。因此这个状态要配上 detail 才有意义——只说「出错」而不说
	// 错在哪，用户唯一的动作是重试。
	BridgeError = "error"
)

// NetworkBridge 对应 network_bridge 表：节点上的一张虚拟网络（F-4-01 / F-4-13）。
//
// 有一句规格上的要求决定了本模块的整体形状：
//
//	**网络配置失败不阻断主流程，但必须可见、可诊断。**
//
// 它的意思是：网络出问题时，面板**仍然要能打开**。这听起来理所当然，
// 但很容易做错——把「探测网络状态」写成一个必须在页面渲染前完成的步骤，
// 那么网络一坏，用户就看到一个白屏或 500，而他本来正是来这里看网络
// 出了什么问题的。因此所有网络探测的**失败都是数据，不是异常**
// （见 service.Overview）。
type NetworkBridge struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_network_bridge_node_name,priority:1;index:idx_network_bridge_node_system,priority:1"`

	Name string `gorm:"size:64;not null;uniqueIndex:uniq_network_bridge_node_name,priority:2"`

	// Backend 记录这张桥是**用哪种后端建的**。
	//
	// 它不是一个可以随便改的偏好设置：换后端意味着拆掉重建。把它记在
	// 这里是为了让「这个节点上哪些桥依赖 OVS」这个问题有答案——而 OVS
	// 缺失时，答案决定了**哪些功能受影响**（f-4-01 明确要求说明这一点）。
	Backend string `gorm:"size:16;not null;default:bridge"`

	Mode string `gorm:"size:16;not null;default:nat"`

	// UplinkIf 是桥绑定的物理口；为空表示**不接外网**。
	//
	// 把它从物理口加到桥上会**重置那个口的 IP 配置**——如果那恰好是管理口，
	// 操作者当场失联，而那时他已经没有任何界面路径可以改回来。
	// 因此这个操作必须显式确认，并且带自动回滚（见 service.AttachUplink）。
	UplinkIf *string `gorm:"size:64"`

	// 列名**必须显式写**：GORM 的命名策略会把 CIDR 拆成 c_id_r
	// （首字母缩写被当成独立单词）。这是同一个坑第四次出现——此前是
	// VCPU → v_cpu、CIDR → c_id_r（多次）。每一次都是列名不一致检查
	// 拦下来的，这正说明那个检查值得留着。
	CIDR      *string `gorm:"column:cidr;size:64"`
	GatewayIP *string `gorm:"size:64"`
	DHCPStart *string `gorm:"size:64"`
	DHCPEnd   *string `gorm:"size:64"`

	DHCPEnabled bool `gorm:"not null;default:false"`
	VlanID      *int `gorm:"column:vlan_id"`

	// IsSystem 标记系统预置的默认网络。
	//
	// 它**不可删除**：新建虚拟机默认接的就是这张桥，删掉它等于让「新建」
	// 这个动作从可用变成不可用，而用户不会立刻把这两件事联系起来。
	IsSystem bool `gorm:"not null;default:false;index:idx_network_bridge_node_system,priority:2"`

	Status string `gorm:"size:16;not null;default:active"`
	// Detail 是最近一次失败的原因（F-4-13 的「可诊断」）。
	//
	// 只说「出错」而不说错在哪，用户唯一的动作是重试——而重试一件已经
	// 失败的事通常不会得到不同结果。
	Detail *string `gorm:"type:text"`

	// UplinkWatchdogUntil 非空表示物理口入桥的**自动回滚窗口仍在计时**。
	//
	// 与端口镜像同一套机制：它是一个「先做了、不确认就撤销」的保险，
	// 而行者在**节点侧**（见 agent.OpNetworkUplinkAttach 的说明）。
	// 这里只记录，用于界面倒计时与状态对齐。
	UplinkWatchdogUntil *time.Time

	Remark *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (NetworkBridge) TableName() string { return "network_bridge" }

// IsUplinked 报告该桥是否接了物理口。
func (b *NetworkBridge) IsUplinked() bool { return b.UplinkIf != nil && *b.UplinkIf != "" }

// IsAwaitingUplinkConfirm 报告该桥是否正处于「等待确认物理口入桥」的窗口里。
func (b *NetworkBridge) IsAwaitingUplinkConfirm() bool {
	return b.IsUplinked() && b.UplinkWatchdogUntil != nil
}

// NeedsOVS 报告该桥是否依赖 Open vSwitch。
//
// 用于在 OVS 缺失时**说明影响了哪些功能**（f-4-01 明确要求不能静默降级）。
func (b *NetworkBridge) NeedsOVS() bool { return b.Backend == BackendOVS }

// ValidBridgeBackend 报告后端取值是否合法。
func ValidBridgeBackend(b string) bool {
	return b == BackendBridge || b == BackendOVS
}

// ValidBridgeMode 报告模式取值是否合法。
func ValidBridgeMode(m string) bool {
	switch m {
	case BridgeModeNAT, BridgeModeRouted, BridgeModeIsolated:
		return true
	}
	return false
}
