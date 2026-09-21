package agent

// 交换机的两个维护动作 + 计数器重置 + IPv6 策略 + 端口释放。
//
// 它们都是**低频但必要**的运维动作：不常点，一旦需要就没有别的路可走
// （例如换物理网卡只能靠迁移，计数器读数不准只能靠重置）。
const (
	// OpVpcSwitchMigrate 把交换机迁移到另一块物理网卡（可同时换 VLAN）。
	//
	// 典型场景：上行网卡换了，或者某个 VLAN 要从一块万兆挪到另一块。它
	// 与 UpdateSwitch 的区别是——后者只改配置，而迁移要**搬动现有端口**。
	OpVpcSwitchMigrate OpKind = "vpc.switch.migrate"
	// OpVpcSwitchReconfigure 按当前配置重新下发一遍。
	//
	// 它对应一个具体场景：面板里的配置是对的，但节点上的实际状态漂了
	// （手工改过、升级后残留）。重配置是幂等的，因此"再点一次"是安全的
	// 第一反应，而不必先去查漂移发生在哪一步。
	OpVpcSwitchReconfigure OpKind = "vpc.switch.reconfigure"
	// OpVpcPortRelease 释放一个 VPC 端口（摘下并回收）。
	OpVpcPortRelease OpKind = "vpc.port.release"
	// OpNetworkCounterReset 重置交换机 / 网口的流量计数。
	//
	// 计数是**累计值**，换过环境或迁移过之后，旧基数会让"这个月用了多少"
	// 完全失真；重置让它从零开始，而不是靠人去记一个差值。
	OpNetworkCounterReset OpKind = "network.counter.reset"
	// OpNetworkIPv6Policy 下发 IPv6 保护策略与可信前缀。
	OpNetworkIPv6Policy OpKind = "network.ipv6.policy"
)

// SwitchActionDataKey 是交换机维护动作结果的键。
const SwitchActionDataKey = "switch_action"

// SwitchActionInfo 是迁移 / 重配置的结果。
type SwitchActionInfo struct {
	// MovedPorts 是迁移时被搬动的端口数。
	MovedPorts int
	// Message 是节点给的说明。
	Message string
	// Rollback 为 true 表示失败且已回滚。
	Rollback bool
}

// PortReleaseDataKey 是端口释放结果的键。
const PortReleaseDataKey = "port_release"

// PortReleaseInfo 是端口释放的结果。
type PortReleaseInfo struct {
	Released bool
	Message  string
}

// CounterResetDataKey 是计数重置结果的键。
const CounterResetDataKey = "counter_reset"

// CounterResetInfo 是计数重置的结果。
type CounterResetInfo struct {
	Reset   int
	Message string
}

// IPv6PolicyDataKey 是 IPv6 策略下发结果的键。
const IPv6PolicyDataKey = "ipv6_policy"

// IPv6PolicyInfo 是 IPv6 策略的结果。
type IPv6PolicyInfo struct {
	Applied bool
	Message string
	// Trusted 是节点实际接受的可信前缀（回显，便于核对有没有被规范化）。
	Trusted []string
}
