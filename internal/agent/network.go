package agent

// OpNodeNetwork 探测节点的网络后端与能力。**只读**。
//
// 能力必须由节点**自报**（f-4-01 R-003）：控制面不得假设某能力存在。
// 宿主机上装没装 OVS、dnsmasq 能不能用，只有节点自己知道；控制面凭
// 「通常都有」去假定，结果是功能报错而不是优雅降级。
const OpNodeNetwork OpKind = "node.network"

// NetworkKey 是 OpNodeNetwork 结果中承载网络后端信息的键。
const NetworkKey = "network"

// ModeBasic 是基础后端模式的标识（Linux 网桥 + dnsmasq + NAT）。
//
// 定义在协议层而非控制面：它是节点自报的取值，两侧必须一致。
const ModeBasic = "basic"

// 网络能力标识。命名沿用「资源域.实现」的风格。
const (
	// CapabilityBridgeBasic Linux 网桥（基础模式的必需能力）。
	CapabilityBridgeBasic = "bridge.basic"
	// CapabilityDHCP dnsmasq 提供的 DHCP/DNS。
	CapabilityDHCP = "dhcp.dnsmasq"
	// CapabilityOVS Open vSwitch。缺失**不阻断服务**（R-004），
	// 只是失去 VPC 相关的进阶能力。
	CapabilityOVS = "network.ovs"
	// CapabilityNAT 宿主 NAT 出网。
	CapabilityNAT = "network.nat"
)

// NetworkBackend 是节点上报的网络后端信息。
type NetworkBackend struct {
	// Mode 是当前生效的后端模式：basic（Linux 网桥 + dnsmasq + NAT）。
	Mode string `json:"mode"`
	// Capabilities 是节点确认具备的能力。
	Capabilities []string `json:"capabilities"`
	// Missing 是节点确认缺失的能力，值为原因。
	//
	// 「缺失」与「未上报」是**两回事**（Q-003）：前者是确认没有，
	// 后者是探测失败。混为一谈会导致误判降级或误判可用，因此分两个字段。
	Missing map[string]string `json:"missing"`
	// ProbeError 非空表示探测本身失败，此时所有能力的状态是「未知」
	// 而不是「缺失」。
	ProbeError string `json:"probe_error,omitempty"`
}
