package model

import (
	"strings"
	"time"
)

// 公网 IP 的可用状态。
const (
	// PublicIPAvailable 已录入池中、当前未绑定。
	PublicIPAvailable = "available"
	// PublicIPBound 已绑定到某台虚拟机。
	PublicIPBound = "bound"
	// PublicIPDisabled 被管理员停用（保留占用但不允许绑定）。
	//
	// 它与「从池中删除」不同：删除会让这个地址重新回到可用池里被别人拿走，
	// 而停用只是暂时不让它被分配——运维摘掉一个出问题的地址时，
	// 需要的是后者。
	PublicIPDisabled = "disabled"
)

// 公网 IP 的绑定模式（f-4-06）。
const (
	// PublicIPModeNAT 1:1 NAT：宿主机做地址转换，来宾无需改配置。
	//
	// 最通用的一种：来宾的网卡配置完全不用动，换绑另一台虚拟机时
	// 目标机也不需要任何准备。代价是流量多经过一跳，且源地址在来宾里
	// 看到的仍是内网地址。
	PublicIPModeNAT = "nat_1to1"
	// PublicIPModeRouted 经典路由：地址直接路由到虚拟机的网卡。
	//
	// 来宾**必须**在自己的网卡上配好这个地址，因此换绑时新机器要先配置。
	// 换来的是没有地址转换——来宾看到的源地址就是真实来源。
	PublicIPModeRouted = "routed"
	// PublicIPModeBridged 经典桥接：地址直接出现在虚拟机的桥上。
	//
	// 来宾需要把地址配在网卡上，且宿主机与来宾要在同一个二层网络里。
	// 它适用于需要在链路上暴露真实 MAC 的场景。
	PublicIPModeBridged = "bridged"
)

// 绑定在节点上的实际生效状态。
//
// 与「有没有绑定记录」是两回事：记录是控制面的**意图**，runtime_status 是
// 宿主机上的**结果**。两者分开才能表达「绑定已受理但节点还没生效」这个
// 中间态——而那个状态对用户是可见的（此时流量还没通）。
const (
	BindingPending = "pending"
	BindingActive  = "active"
	BindingFailed  = "failed"
)

// PublicIP 对应 public_ip 表：节点上的公网地址池（F-4-06）。
//
// 地址是**节点内唯一**的（uniq_public_ip_node_ip）：同一个地址在两台宿主机
// 上出现会让流量随机落到其中一台，而那种问题从现象上几乎看不出来。
type PublicIP struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_public_ip_node_ip,priority:1;index:idx_public_ip_node_status,priority:1"`
	// IP 是地址本身；CIDR / Gateway / EgressIf 描述它所在的那个网段怎么走。
	IP string `gorm:"column:ip;size:64;not null;uniqueIndex:uniq_public_ip_node_ip,priority:2"`
	// CIDR 是该地址所在的网段（如 203.0.113.0/24）。
	//
	// 录入整段时**每个地址一行**，而不是存一行网段再在绑定时切分：
	// 绑定、停用、迁移都是针对单个地址的操作，拆成行之后这些操作
	// 就只是改一行，而不必去解析与改写网段。
	// 列名**必须显式写**：GORM 的命名策略会把 CIDR 拆成 c_id_r
	// （首字母缩写被当成独立单词），与迁移里的 cidr 对不上。
	// 这是同一个坑第二次出现（此前是 VCPU → v_cpu）。
	CIDR    *string `gorm:"column:cidr;size:64"`
	Gateway *string `gorm:"size:64"`
	// EgressIf 是出网物理网卡。
	EgressIf *string `gorm:"size:64"`

	AddressFamily string `gorm:"size:8;not null;default:ipv4"`
	// SupportedModes 是该地址**支持**的绑定模式（逗号分隔）。
	//
	// 不是每个地址都能用每种模式：IPv6 通常没有 NAT（地址足够多，
	// 直接路由即可），而某些上游只给了一条静态路由。让用户去试错
	// 会得到一句来自内核的报错，而不是「这个地址不支持这种模式」。
	SupportedModes *string `gorm:"size:128"`

	Status string  `gorm:"size:16;not null;default:available;index:idx_public_ip_node_status,priority:2"`
	Remark *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (PublicIP) TableName() string { return "public_ip" }

// Modes 返回该地址支持的模式列表；未声明时返回全部。
//
// 未声明时**不返回空**：老数据（或手工录入的行）可能没有这一列，
// 返回空会让它一个模式都用不了，而「能用但可能配错」比「莫名其妙不能用」
// 更容易被发现——绑定失败时节点会给出一句明确的报错。
func (p *PublicIP) Modes() []string {
	if p.SupportedModes == nil || *p.SupportedModes == "" {
		return []string{PublicIPModeNAT, PublicIPModeRouted, PublicIPModeBridged}
	}
	var out []string
	for _, m := range strings.Split(*p.SupportedModes, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return []string{PublicIPModeNAT, PublicIPModeRouted, PublicIPModeBridged}
	}
	return out
}

// SupportsMode 报告该地址是否支持某种绑定模式。
func (p *PublicIP) SupportsMode(mode string) bool {
	for _, m := range p.Modes() {
		if m == mode {
			return true
		}
	}
	return false
}

// PublicIPBinding 对应 public_ip_binding 表：一条绑定记录（F-4-06）。
//
// **一个地址同一时刻只能有一条有效绑定**（uniq_public_ip_binding_active）。
// 这是网络层的事实而不是控制面的偏好：一个公网地址只能指向一个地方，
// 两处同时宣告同一个地址会让流量随机落到其中一边。
//
// 索引上的 `where:released_at IS NULL` **不能省**。建库时它没有这个条件，
// 于是唯一性落到了「一个地址总共只能有一条绑定记录」上——那会让浮动迁移
// 必然失败（迁移要释放旧的、建立新的），也让「保留绑定历史」在设计上
// 就不可能。而历史恰恰是公网地址最需要留住的东西：「这个地址在某个时间点
// 指向谁」是安全审计与故障排查的核心信息。
//
// 也正因如此，**浮动迁移必须是「解绑旧绑定、建立新绑定」的原子操作**，
// 而不能是「再加一条绑定」——后者会被数据库直接拒绝。
type PublicIPBinding struct {
	ID int64 `gorm:"primaryKey"`
	// 唯一性**只针对有效绑定**（见类型注释）：部分唯一索引让「一个地址
	// 同时只指向一处」与「保留历史」两件事同时成立。
	PublicIPID int64 `gorm:"not null;uniqueIndex:uniq_public_ip_binding_active,where:released_at IS NULL"`
	NodeID     int64 `gorm:"not null"`
	// VMID 为空表示该地址已被占用但尚未指向任何虚拟机（预留）。
	//
	// 预留是真实需求：用户先占住一个地址、稍后再挂到机器上。
	// 没有这个状态时，他只能在真正要用的那一刻才抢地址，
	// 而那时地址可能已经被别人拿走了。
	VMID *int64 `gorm:"index:idx_public_ip_binding_vm_id"`

	Mode string `gorm:"size:24;not null"`
	// RuntimeStatus 是节点上的实际生效状态；见 BindingPending 等常量。
	RuntimeStatus string `gorm:"size:16;not null;default:pending"`

	BoundAt *time.Time
	// ReleasedAt 在解绑时写入。
	//
	// 记录不与地址一起删除，而是留着并标记释放时间：事后追查「这个地址
	// 在某个时间点指向谁」时，唯一能回答的就是这条历史。删掉它，
	// 那段历史就无从查起。
	ReleasedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (PublicIPBinding) TableName() string { return "public_ip_binding" }

// IsActive 报告该绑定是否仍然有效（未被释放）。
func (b *PublicIPBinding) IsActive() bool { return b.ReleasedAt == nil }

// PublicIPModeLabel 把模式翻译成用户能读懂的说法。
func PublicIPModeLabel(mode string) string {
	switch mode {
	case PublicIPModeNAT:
		return "1:1 NAT（来宾无需改配置）"
	case PublicIPModeRouted:
		return "经典路由（来宾需自行配置该地址）"
	case PublicIPModeBridged:
		return "经典桥接（来宾需在同一二层网络）"
	default:
		return mode
	}
}
