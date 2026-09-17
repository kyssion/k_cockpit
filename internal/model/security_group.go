package model

import (
	"time"

	"gorm.io/gorm"
)

// 规则方向。
const (
	DirectionIngress = "ingress"
	DirectionEgress  = "egress"
)

// 规则协议。`all` 表示不限协议（同时覆盖 TCP/UDP/ICMP）。
const (
	ProtocolTCP  = "tcp"
	ProtocolUDP  = "udp"
	ProtocolICMP = "icmp"
	ProtocolAll  = "all"
)

// 规则的**目标类型**（f-4-03：CIDR / 交换机 / 安全组）。
//
// 目标可以是另一个安全组，这是安全组能组合的前提：一条「允许来自「Web 层」
// 组的流量」的规则，会把该组**所有成员**展开进来，而成员增减会自动反映到
// 规则上——不必每加一台机器就去改十份规则。
const (
	TargetCIDR   = "cidr"
	TargetSwitch = "switch"
	TargetGroup  = "group"
)

// SecurityGroup 对应 security_group 表：一组入站/出站规则（F-4-03）。
//
// **组内只有「允许」规则，没有「拒绝」。** 表结构里根本没有 action 列，
// 这不是遗漏：
//
// 安全组是**叠加生效**的——一台机器挂多个组时，生效规则是各组的并集。
// 在一个「并集」模型里，拒绝规则是没法定义的：A 组拒绝 22 端口、B 组允许
// 22 端口，合并之后到底是通还是不通？答案取决于谁先算，而「谁先算」是
// 用户看不见的实现细节。
//
// 因此**默认拒绝由组的整体语义给出**（不在任何允许规则里的流量一律不通），
// 而不是由一条显式的拒绝规则给出。要「只放行特定来源」，做法是只写那几条
// 允许规则，而不是写一条「拒绝其他」——后者在叠加下没有确定含义。
//
// security_group_rule.priority 因此**不影响判定结果**，它只决定规则的展示
// 顺序与下发到宿主机后的书写顺序。把它当成「优先级高的先匹配」会得到一个
// 与实现不符的心智模型。
type SecurityGroup struct {
	ID int64 `gorm:"primaryKey"`
	// 唯一索引带 `where:deleted_at IS NULL`，与迁移 0017 一致。
	//
	// **条件不能省**：不给条件时索引落到了全部行上，而本表有 deleted_at
	// （软删除）——被删除的组会永久占住名字，用户删掉「web」后重建同名的
	// 会撞上一句他无法理解的唯一约束冲突。而这一条只影响**测试库**：
	// AutoMigrate 按模型建表，模型少了条件，测试库就与真实库分叉了。
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_security_group_node_name,priority:1,where:deleted_at IS NULL"`
	// OwnerID 为空表示系统预置组（管理员创建、不属于任何租户）。
	//
	// 允许为空而不是挂到一个「系统用户」上：预置组不该出现在任何人的
	// 资源列表里，也不该随着某个账号被删除而失去归属。
	OwnerID *int64 `gorm:"index:idx_security_group_owner_id"`

	Name string `gorm:"size:64;not null;uniqueIndex:uniq_security_group_node_name,priority:2,where:deleted_at IS NULL"`
	// IsDefault 表示新建网口时默认挂载的组。
	//
	// 唯一性**不做数据库约束**：默认组是「每节点至多一个」还是「允许没有」
	// 属于业务规则，而迁移里并没有为它建索引。这里只声明语义，由服务层
	// 在设置时把旧默认清掉——不加约束的话并发设置可能短暂出现两个，
	// 但那只会让「默认挂哪个」在最坏情况下不确定，不会损坏数据。
	IsDefault bool    `gorm:"not null;default:false"`
	Remark    *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt 启用软删除。
	//
	// **唯一索引必须带 `WHERE deleted_at IS NULL`**（见迁移 0017）：不带的话
	// 删掉的组会永久占住名字，用户重建同名组时会撞上一句唯一约束冲突，
	// 而他刚刚明明把这个名字删掉了——那句报错他无法理解，也无法自行解决。
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

// TableName 固定表名。
func (SecurityGroup) TableName() string { return "security_group" }

// SecurityGroupRule 对应 security_group_rule 表：组内的一条**允许**规则。
type SecurityGroupRule struct {
	ID      int64 `gorm:"primaryKey"`
	GroupID int64 `gorm:"not null;index:idx_security_group_rule_group_id"`
	// Direction 取值 ingress / egress。
	Direction string `gorm:"size:8;not null"`
	// Protocol 取值 tcp / udp / icmp / all。
	Protocol string `gorm:"size:8;not null;default:tcp"`
	// PortStart / PortEnd 为空表示不限端口。
	//
	// 单端口时两者相等，而不是只填 start 让 end 留空——「等于」与「不限」
	// 是两种不同的意思，让它们长得不一样可以在读数据时立刻分辨。
	// ICMP 与 all 协议下端口无意义，服务层会拒绝填写。
	PortStart *int `gorm:"column:port_start"`
	PortEnd   *int `gorm:"column:port_end"`

	// TargetType / TargetValue 描述这条规则作用于谁（见 TargetCIDR 等）。
	//
	// TargetValue 为空是合法且有含义的：目标类型为 switch 或 group 时，
	// 具体的对象由 TargetValue 指向；而某些情况下它就是「全部」。
	TargetType  string  `gorm:"size:16;not null;default:cidr"`
	TargetValue *string `gorm:"size:64"`

	AddressFamily string `gorm:"size:8;not null;default:ipv4"`
	// Priority 只影响**展示顺序**，不影响判定（见 SecurityGroup 的说明）。
	Priority int     `gorm:"not null;default:100"`
	Remark   *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (SecurityGroupRule) TableName() string { return "security_group_rule" }

// InterfaceSecurityGroup 对应 interface_security_group 表：网口与**附加**安全组
// 的关联。
//
// 为什么需要它：`vm_interface.security_group_id` 是单个外键，只能表达
// 「一个网口挂一个组」，而 f-4-03 要求**多组叠加生效**。
//
// 主组为什么不一起迁到这张表：网口的编辑流程已经在用那个字段。迁移会让
// 那部分代码同时改动，而这次的目的是让**多组**成为可能，不是重构网口模型。
// 生效规则 = 主组 ∪ 附加组，两者在语义上完全平等，只是存放位置不同。
type InterfaceSecurityGroup struct {
	ID          int64 `gorm:"primaryKey"`
	InterfaceID int64 `gorm:"not null;uniqueIndex:uniq_interface_security_group,priority:1"`
	GroupID     int64 `gorm:"not null;uniqueIndex:uniq_interface_security_group,priority:2"`

	CreatedAt time.Time
}

// TableName 固定表名。
func (InterfaceSecurityGroup) TableName() string { return "interface_security_group" }

// ValidProtocol 报告协议取值是否合法。
func ValidProtocol(p string) bool {
	switch p {
	case ProtocolTCP, ProtocolUDP, ProtocolICMP, ProtocolAll:
		return true
	}
	return false
}

// ProtocolUsesPorts 报告该协议下端口是否有意义。
//
// ICMP 没有端口概念，`all` 覆盖全部协议因而也不能限定端口。允许给它们填
// 端口会生成一条**看起来有约束、实际没有**的规则——用户以为只放行了
// 22 端口，实际整个协议都通着，而这种偏差不会以任何形式报错。
func ProtocolUsesPorts(p string) bool {
	return p == ProtocolTCP || p == ProtocolUDP
}
