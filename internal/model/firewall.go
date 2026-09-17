package model

import "time"

// 防火墙动作。
const (
	FirewallAccept = "accept"
	FirewallDeny   = "deny"
)

// FirewallPolicy 对应 firewall_policy 表：**节点级**防火墙策略（F-4-11）。
//
// 双层结构的第一层。它作用在该节点上的所有虚拟机入站流量上，是一份
// 「默认基线」；单台虚拟机可以用 FirewallVMPolicy 做覆盖。
//
// 为什么要有节点级这一层，而不是每台机器各配一份：绝大多数机器需要的是
// 同一套基线（放行 SSH、放行 ICMP、其余拒绝）。逐台配置意味着新增一台
// 机器时默认是**无保护**的——而「忘了配」这件事不会以任何形式报警，
// 直到出事。有一份基线，新机器天然受保护。
type FirewallPolicy struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_firewall_policy_node_id"`

	Enabled bool `gorm:"not null;default:false"`
	// DefaultAction 是不匹配任何规则时的处置。
	//
	// 默认是 deny（与建库时的定义一致）：防火墙的价值就在于**默认拒绝**，
	// 而 default accept 只是一组「特定来源不许进」的例外清单——它能防的
	// 事情比它看起来能防的少得多。
	DefaultAction string `gorm:"size:8;not null;default:deny"`

	// GeoipRegions 是**允许**的区域列表（逗号分隔的国家/地区码）。
	//
	// 留空表示不按区域限制。填了之后，不在列表里的来源一律拒绝——**但
	// 白名单优先**（见 Whitelist）。
	GeoipRegions *string `gorm:"size:512"`

	// Whitelist 是**永远放行**的来源（CIDR，逗号或换行分隔）。
	//
	// 白名单优先于一切拒绝规则，包括区域限制。这条优先级不是便利性设计，
	// 而是安全底线：管理员从某个固定 IP 管理面板，而那个 IP 万一落在被
	// 区域规则挡掉的范围里（用了代理、或区域数据不准），**他会把自己锁在
	// 门外**——而那时他已经连不上面板去改回来了。
	Whitelist *string `gorm:"type:text"`

	// Version 每次改动自增，供「预览 → 应用」校验（f-4-04 同一模式）。
	Version int `gorm:"not null;default:0"`

	// AppliedAt 是最近一次成功下发到节点的时刻。
	//
	// 为空表示「有配置但没生效过」。它与 version 分开：version 是控制面的
	// 配置版本，applied_at 是宿主机的实际状态——两者不一致时用户需要知道
	// 「我改的东西还没生效」。
	AppliedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (FirewallPolicy) TableName() string { return "firewall_policy" }

// FirewallRule 对应 firewall_rule 表：一条节点级规则（F-4-11）。
type FirewallRule struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;index:idx_firewall_rule_node_protected,priority:1"`

	// 去重**不在模型上声明索引**，由服务层保证（见 firewall.duplicateOf）。
	//
	// 迁移里的 uniq_firewall_rule_dedup 是个**表达式索引**：
	//   (node_id, action, protocol, coalesce(port_start,0),
	//    coalesce(port_end,0), coalesce(source_cidr,''))
	// 用 coalesce 是因为唯一索引里 NULL 互不相等，而两条「端口与来源都留空」
	// 的规则显然是同一条——不归一化的话去重根本不成立。
	//
	// GORM 表达不了它：索引选项以逗号分隔，而 coalesce 的参数里就有逗号，
	// 写进 tag 会生成残缺的 SQL（AutoMigrate 直接报错）。因此改成**服务层
	// 比对**，并把这条索引作为已知例外登记在 index_alignment_test.go 里。
	//
	// 服务层比对与数据库约束**不是等价的两件事**：并发写入仍可能挤进两条。
	// 但规则是管理员手工添加的，这个窗口可以接受，而代价换来的是测试库
	// 与真实库在这一点上的一致行为。
	Action   string `gorm:"size:8;not null"`
	Protocol string `gorm:"size:8;not null;default:tcp"`
	// PortStart / PortEnd 为空表示不限端口。
	//
	// 去重索引用 coalesce(port_start, 0) 把 NULL 归一化——两个都为空的规则
	// 才是同一条，而 NULL 在唯一索引里默认互不相等。见 model 上的索引声明。
	PortStart *int `gorm:"column:port_start"`
	PortEnd   *int `gorm:"column:port_end"`

	// SourceCIDR 为空表示任意来源。
	// 列名**必须显式写**：GORM 的命名策略会把 CIDR 拆成 c_id_r
	// （首字母缩写被当成独立单词），与迁移里的 source_cidr 对不上。
	// 这已经是同一个坑第三次出现（此前是 VCPU → v_cpu、CIDR → c_id_r）。
	SourceCIDR *string `gorm:"column:source_cidr;size:64"`

	// IsProtected 标记**不可被界面修改或删除**的规则。
	//
	// 这类规则保护的是管理通道本身：SSH 端口、面板端口、以及端口转发自动
	// 生成的放通规则。它们必须存在，否则一次「清理规则」的操作就能把管理员
	// 关在门外——而那种事故无法通过面板恢复，只能上宿主机敲命令。
	//
	// 因此**保护是服务端强制的**，界面上的标红只是提示。让界面决定能不能删，
	// 等于把一个不可恢复的操作交给一次点击。
	IsProtected bool `gorm:"not null;default:false;index:idx_firewall_rule_node_protected,priority:2"`

	// OrderNo 决定规则在宿主机链里的顺序。
	//
	// 在「默认拒绝 + 允许清单」模型里，顺序**不影响判定结果**（要么被某条
	// 允许命中，要么落到默认拒绝）。它只影响日志与计数器的可读性——不过
	// 明确一条规则的相对位置，对排查「为什么这条没生效」仍然有用。
	OrderNo int `gorm:"not null;default:100"`

	// Applied 标记该规则是否已下发到宿主机。
	//
	// 与 policy.version 不同：那条说明整份策略的版本，这条说明**单条规则**
	// 是否落到了宿主机上。新增一条规则后没下发时，界面要能指出是哪一条
	// 还没生效，而不是笼统地说「有改动未应用」。
	Applied bool `gorm:"not null;default:false"`

	Remark *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (FirewallRule) TableName() string { return "firewall_rule" }

// FirewallVMPolicy 对应 firewall_vm_policy 表：**单台虚拟机**的覆盖策略。
//
// 双层结构的第二层。它是在节点级基线之上的一层覆盖：
//
//   - Action 为 deny 时，这台机器**不受节点级允许规则的放行**（比基线更严）
//   - 有自己的区域与白名单时，用它们替换节点级的（而不是叠加）
//
// 「替换而不是叠加」是刻意的：叠加会让「这台机器到底受哪些约束」变成两个
// 列表的并集，而用户在排查时需要在两份配置之间来回对照。替换让这台机器的
// 策略是**自包含**的——看这一条就够了。
type FirewallVMPolicy struct {
	ID   int64 `gorm:"primaryKey"`
	VMID int64 `gorm:"not null;uniqueIndex:uniq_firewall_vm_policy_vm_id"`
	// PolicyID 指向所覆盖的节点级策略；为空表示使用所属节点的策略。
	PolicyID *int64

	Action       string  `gorm:"size:8;not null;default:accept"`
	GeoipRegions *string `gorm:"size:512"`
	Whitelist    *string `gorm:"type:text"`
	Enabled      bool    `gorm:"not null;default:true"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (FirewallVMPolicy) TableName() string { return "firewall_vm_policy" }

// ValidFirewallAction 报告动作取值是否合法。
func ValidFirewallAction(a string) bool {
	return a == FirewallAccept || a == FirewallDeny
}

// ValidFirewallProtocol 报告协议取值是否合法。
func ValidFirewallProtocol(p string) bool {
	switch p {
	case ProtocolTCP, ProtocolUDP, ProtocolICMP, ProtocolAll:
		return true
	}
	return false
}
