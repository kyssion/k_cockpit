package model

import "time"

// 端口安全策略的状态。
const (
	// PortSecurityPending 已配置但尚未在节点上生效。
	PortSecurityPending = "pending"
	// PortSecurityActive 已在节点上生效。
	PortSecurityActive = "active"
	// PortSecurityFailed 下发失败。
	PortSecurityFailed = "failed"
)

// PortSecurityPolicy 对应 port_security_policy 表：一个网口的端口安全策略（F-4-08）。
//
// 三项能力回答三个不同的问题，而它们的**方向并不一致**——这一点值得先写下来：
//
//	SpoofingGuard  「这个网口只能用它自己的地址」——收窄它能做的事
//	Isolation      「这个网口不能和同网段的邻居说话」——也收窄，但代价更大
//	PPSLimit       「这个网口每秒最多发多少个包」——限流
//
// 前两项是**安全边界**，第三项是**资源保护**。它们的默认值应当不同：安全
// 性的默认应当是"开"，而限流的默认应当是"不限"——一个默认限流的网络会在
// 用户什么都没做的时候就开始丢包，而那种丢包看起来像应用的问题。
type PortSecurityPolicy struct {
	ID int64 `gorm:"primaryKey"`
	// NodeID + PortRef 唯一（uniq_port_security_policy_node_port）。
	//
	// 一个网口一份策略：允许两份的话，两份会在节点上互相覆盖，而结果
	// 取决于下发顺序——那是用户看不见的实现细节。
	NodeID int64 `gorm:"column:node_id;not null;uniqueIndex:uniq_port_security_policy_node_port,priority:1"`
	// PortRef 是网口引用（网桥名 / 虚拟机网口名）。
	PortRef string `gorm:"column:port_ref;size:128;not null;uniqueIndex:uniq_port_security_policy_node_port,priority:2"`

	SwitchID *int64 `gorm:"column:switch_id"`
	VMID     *int64 `gorm:"column:vm_id"`

	// SpoofingGuard 阻止该网口发出不属于它的源 IP / 源 MAC。
	//
	// **这是整块功能里最要紧的一项，因为它是别的隔离措施的前提。**
	//
	// 一个虚拟机如果把源 IP 伪造成邻居的地址，那么针对那台邻居做的隔离、
	// 防火墙、端口转发全部会指向错误的机器。换句话说：不防欺骗的话，
	// 网络里所有"按地址区分机器"的策略都可以被绕过。
	SpoofingGuard bool `gorm:"column:spoofing_guard;not null;default:false"`
	// Isolation 阻止该网口与其所在的二层网段内其它端口互通。
	//
	// 代价比它看起来大：**同网段内所有机器之间都不通了**，包括用户自己
	// 放在一起的应用集群。交换机变成"只通网关"。因此开启前必须明确告知，
	// 而不是当成一个无害的开关。
	Isolation bool `gorm:"column:isolation;not null;default:false"`
	// PPSLimit 是每秒包数上限，0 表示不限。
	//
	// 它依赖 OVS meter（`network.ovs.meter`）——**没有这个能力时不能假装
	// 支持**：配了一个不生效的上限，用户会以为攻击面已经被收住了。
	PPSLimit int `gorm:"column:pps_limit;not null;default:0"`

	Status string `gorm:"size:16;not null;default:pending"`
	// Detail 是节点回的说明或失败原因。
	Detail *string `gorm:"type:text"`

	// AppliedAt 是节点上**确认生效**的时刻；为空表示尚未生效。
	AppliedAt *time.Time `gorm:"column:applied_at"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (PortSecurityPolicy) TableName() string { return "port_security_policy" }

// IsActive 报告策略是否已在节点上生效。
func (p *PortSecurityPolicy) IsActive() bool { return p.Status == PortSecurityActive }

// Enabled 报告这份策略是否至少开了一项保护。
func (p *PortSecurityPolicy) Enabled() bool {
	return p.SpoofingGuard || p.Isolation || p.PPSLimit > 0
}

// NeedsMeter 报告这份策略是否用到了需要 OVS meter 的限速。
func (p *PortSecurityPolicy) NeedsMeter() bool { return p.PPSLimit > 0 }
