package model

import "time"

// ACL 动作与方向。
const (
	AclActionAllow = "allow"
	AclActionDeny  = "deny"

	AclDirectionIn  = "in"
	AclDirectionOut = "out"

	// AclProtocolAny 表示不区分协议（tcp / udp / icmp 之外）。
	AclProtocolAny = "any"
)

// VpcACLRule 是一条 VPC 网络的访问控制规则（F-4-05）。
//
// 它与安全组规则的区别在于**作用域**：安全组挂在虚拟机网口上，ACL 挂在
// 交换机（一个网段）上。因此 ACL 适合表达"这个网段整体上不许出去访问
// 某个地址"这类跨机器的约束，而逐台配安全组既重复又容易漏。
//
// 与防火墙规则的区别在于**谁在用**：防火墙是宿主机 iptables / nftables，
// ACL 只作用于本交换机的二层/三层转发。混用会让"为什么这条不生效"变成
// 一个要同时看两处才能回答的问题，因此两者在界面上是分开的两页。
type VpcACLRule struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;index:idx_vpc_acl_switch"`
	// SwitchID 为空表示这条是**节点级默认**（作用于该节点的全部 VPC 网络）。
	SwitchID *int64 `gorm:"index:idx_vpc_acl_switch"`

	Priority int    `gorm:"not null;default:100"`
	Action   string `gorm:"size:8;not null"`
	// Direction 为 in 时匹配进入该网段的流量，out 为离开。
	Direction string `gorm:"size:4;not null;default:in"`
	Protocol  string `gorm:"size:8;not null;default:any"`

	SrcCIDR *string `gorm:"column:src_cidr;size:64"`
	DstCIDR *string `gorm:"column:dst_cidr;size:64"`

	PortStart *int `gorm:"column:port_start"`
	PortEnd   *int `gorm:"column:port_end"`

	Enabled bool    `gorm:"not null;default:true"`
	Remark  *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VpcACLRule) TableName() string { return "vpc_acl_rule" }

// MatchesAll 报告这条规则是否匹配全部地址。
//
// 它用于给"拒绝一切"这类规则加提示：一条匹配全部且动作为 deny 的规则若
// 排在前面，后面的规则永远不会生效——而那正是"设了白名单却全不通"最常见
// 的原因。
func (r *VpcACLRule) MatchesAll() bool {
	return (r.SrcCIDR == nil || *r.SrcCIDR == "") && (r.DstCIDR == nil || *r.DstCIDR == "")
}
