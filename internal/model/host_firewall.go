package model

import "time"

// HostFirewallPolicy 对应 host_firewall_policy 表：宿主机防火墙的节点级策略
// （F-4-11 第一层）。
//
// **它与 FirewallPolicy 是两套东西。** 后者管的是虚拟机的入站流量，而这一份
// 保护的是**宿主机自己与面板**——SSH、面板端口、节点上直接对外的服务。
//
// 为什么用独立的类型与表，而不是给 FirewallPolicy 加一个 layer 字段：
// 这两个概念最容易混为一谈，而混在一起的后果不是代码难看，是**用户会以为
// 改了 KVM 规则就关掉了面板的暴露面**——那是两个完全不同的攻击面，而它们的
// 配置项长得几乎一样。
type HostFirewallPolicy struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"column:node_id;not null;uniqueIndex:uniq_host_firewall_policy_node_id"`

	Enabled bool `gorm:"not null;default:false"`
	// DefaultAction 是不匹配任何规则时的处置。
	//
	// 默认 deny：防火墙的价值就在于**默认拒绝**，而 default accept 只是一组
	// 「特定来源不许进」的例外清单——它能防的事情比它看起来能防的少得多。
	DefaultAction string `gorm:"column:default_action;size:8;not null;default:deny"`

	// Whitelist 是**永远放行**的来源（CIDR，逗号或换行分隔）。
	//
	// 优先于一切拒绝规则。这条优先级不是便利性设计，而是安全底线：
	// 宿主机防火墙挡住的**正是 SSH 与面板本身**，没有任何"从里面绕过去"
	// 的余地——管理员一旦被自己的规则挡在外面，就只剩进机房这一条路。
	Whitelist *string `gorm:"type:text"`

	// Version 每次改动自增，供「预览 → 应用」校验。
	Version int `gorm:"not null;default:0"`

	// AppliedAt 为空表示「有配置但没生效过」。
	AppliedAt *time.Time `gorm:"column:applied_at"`

	// LastRollbackAt 是最近一次紧急回滚的时刻。
	//
	// 回滚要留痕：它意味着"刚才那次应用把机器弄坏了"。事后看审计能查到是谁
	// 点的，而这一列让界面直接说出「这条策略最近被回滚过」——那正是排查
	// "为什么规则和我配的不一样"时第一个要看的东西。
	LastRollbackAt *time.Time `gorm:"column:last_rollback_at"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (HostFirewallPolicy) TableName() string { return "host_firewall_policy" }

// HostFirewallRule 对应 host_firewall_rule 表：一条宿主机防火墙规则。
type HostFirewallRule struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"column:node_id;not null;index:idx_host_firewall_rule_node_protected,priority:1"`

	Action   string `gorm:"size:8;not null"`
	Protocol string `gorm:"size:8;not null;default:tcp"`

	// PortStart / PortEnd 为空表示不限端口。
	PortStart *int `gorm:"column:port_start"`
	PortEnd   *int `gorm:"column:port_end"`

	// SourceCIDR 为空表示任意来源。
	//
	// 列名**必须显式写**：GORM 的命名策略会把 CIDR 拆成 c_id_r。
	// 这个坑已经出现过四次（VCPU、CIDR、source_cidr、现在这里）。
	SourceCIDR *string `gorm:"column:source_cidr;size:64"`

	// GeoipRegions 是该规则允许的区域（逗号分隔的国家码）。
	GeoipRegions *string `gorm:"column:geoip_regions;size:512"`

	// IsProtected 标记**不可被界面修改或删除**的规则。
	//
	// 这类规则保护的是管理通道本身：面板自己的监听端口、SSH 端口。
	// 删掉它们等于把管理员关在门外，而那种事故**无法通过面板恢复**。
	//
	// 保护是**服务端强制**的，界面上的标红只是提示。
	IsProtected bool `gorm:"column:is_protected;not null;default:false;index:idx_host_firewall_rule_node_protected,priority:2"`

	// Applied 标记该规则是否已下发到宿主机。
	Applied bool `gorm:"not null;default:false"`

	Remark *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (HostFirewallRule) TableName() string { return "host_firewall_rule" }

// Describe 用一句人话描述这条规则匹配什么，供界面与审计使用。
func (r *HostFirewallRule) Describe() string {
	out := r.Protocol
	if r.PortStart != nil {
		out += " " + itoa(*r.PortStart)
		if r.PortEnd != nil && *r.PortEnd != *r.PortStart {
			out += "-" + itoa(*r.PortEnd)
		}
	} else {
		out += " 全部端口"
	}
	if r.SourceCIDR != nil && *r.SourceCIDR != "" {
		out += " 来自 " + *r.SourceCIDR
	} else {
		out += " 来自任意来源"
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
