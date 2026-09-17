package model

import "time"

// 网络模式（vpc_switch.mode）。
const (
	// NetworkModeEmpty 隔离网络：无上行，虚拟机之间可互通。
	NetworkModeEmpty = "empty"
	// NetworkModePhysical 桥接到物理网卡。
	NetworkModePhysical = "physical"
	// NetworkModeNAT 通过宿主做 NAT 出网。M2 的系统基础网络使用它。
	NetworkModeNAT = "nat"
)

// 网络状态。
const (
	NetworkActive = "active"
	// NetworkError 表示运行态异常（如网桥被外部删除）。
	//
	// 标记异常而**不静默重建**（f-4-01 Q-009）：静默重建会与人工维护
	// 「打架」——管理员正在手工调整网络，系统又把它改回去，而后者更难
	// 排查，因为它看起来「自己好了」。
	NetworkError = "error"
)

// NetworkDefaultName 是系统基础网络的名称。
const NetworkDefaultName = "default"

// VpcSwitch 对应 vpc_switch 表。
//
// M2 只使用 `IsSystem = true` 的系统基础网络记录；VPC 交换机的其余字段
// （带宽、流量配额等）由 M3 启用（f-4-01 §5.1）。
type VpcSwitch struct {
	ID int64 `gorm:"primaryKey"`
	// NodeID 参与两个唯一索引：同节点内交换机名唯一、VLAN ID 唯一。
	NodeID  int64 `gorm:"not null;uniqueIndex:uniq_vpc_switch_node_name,priority:1;uniqueIndex:uniq_vpc_switch_node_vlan,priority:1"`
	OwnerID *int64
	Name    string `gorm:"size:64;not null;uniqueIndex:uniq_vpc_switch_node_name,priority:2"`
	Mode    string `gorm:"size:16;not null;default:empty"`
	// BridgeName 是宿主机上的网桥名。
	BridgeName string `gorm:"size:64;not null"`
	// VlanID 在节点内唯一（uniq_vpc_switch_node_vlan），可为空。
	// 空值不参与唯一性判定，因此多个不划 VLAN 的交换机不会互相冲突。
	VlanID *int `gorm:"column:vlan_id;uniqueIndex:uniq_vpc_switch_node_vlan,priority:2"`
	// column 必须显式声明：GORM 的命名策略把 `CIDR` 转成了 `c_id_r`
	// （`CIDR` 不在它的常见缩写词表里，被按大写边界切开了），而迁移建的列
	// 叫 `cidr`。详见 model/vm.go 中 VCPU 的同款说明。
	CIDR      *string `gorm:"column:cidr;size:64"`
	GatewayIP *string `gorm:"size:64"`
	DHCPStart *string `gorm:"size:64"`
	DHCPEnd   *string `gorm:"size:64"`
	UplinkIf  *string `gorm:"size:64"`

	BandwidthInMbps  int `gorm:"not null;default:0"`
	BandwidthOutMbps int `gorm:"not null;default:0"`

	// IsSystem 标记系统基础网络。
	//
	// 它**不可删除**（R-006 / Q-007）：保证「任何节点上总有可用的网络」，
	// 避免用户误删后无法创建虚拟机。M3 的 VPC 交换机是可删的，系统网络不是。
	IsSystem bool    `gorm:"not null;default:false"`
	Status   string  `gorm:"size:16;not null;default:active"`
	Remark   *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
	// DeletedAt 为软删除。系统网络不会被删除，但普通交换机需要保留记录
	// 供审计引用。
	DeletedAt *time.Time
}

// TableName 固定表名。
func (VpcSwitch) TableName() string { return "vpc_switch" }

// 网卡型号。取值与 QEMU 的设备类型对应。
const (
	// NICModelVirtio 是默认型号：半虚拟化，性能最好，需要来宾有对应驱动。
	NICModelVirtio = "virtio"
	// NICModelE1000 是 Intel 千兆网卡，兼容性最好，用于没有 virtio 驱动的
	// 老旧系统（装完系统没网卡，问题通常就出在这里）。
	NICModelE1000 = "e1000"
	// NICModelRTL8139 用于更老的系统（Windows XP 一类的）。
	NICModelRTL8139 = "rtl8139"
)

// 地址族。
const (
	AddressFamilyIPv4 = "ipv4"
	AddressFamilyIPv6 = "ipv6"
)

// VMInterface 对应 vm_interface 表：一台虚拟机的网卡。
//
// 与多数「改了立刻生效」的配置不同，网卡变更需要**下发到节点**才生效，因此
// 本表带 LastAppliedAt：它是投影，可能与虚拟化层的实际配置不一致。为空表示
// 从未下发成功，界面据此提示「尚未生效」，而不是假装已经改好了。
type VMInterface struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"not null;uniqueIndex:uniq_vm_interface_vm_order,priority:1"`
	NodeID int64 `gorm:"not null"`

	// Order 是网卡序号，从 0 开始。**它同时是网卡在虚拟机内的标识**：
	// 来宾系统看到的设备顺序由它决定。数据库 id 反而不可靠——重建网卡时
	// 会得到新 id，而来宾里的 eth0/eth1 是按顺序认的。
	//
	// `order` 是 SQL 保留字，由 GORM 自动加引号（迁移里也是带引号建的）。
	// 索引与迁移保持一致（uniq_vm_interface_vm_order），否则测试库与真实库
	// 的约束会分叉——那正是上次「测试全绿、生产必挂」的成因。
	Order int `gorm:"column:order;not null;uniqueIndex:uniq_vm_interface_vm_order,priority:2"`

	// IsPrimary 标记主网卡。主网卡不可删除——重装系统（f-2-11）依赖它保持
	// 网络可达，删掉它就没有恢复路径了。
	IsPrimary bool `gorm:"not null;default:false"`

	// SwitchID 指向 VpcSwitch；为空表示使用节点默认网络。
	SwitchID        *int64 `gorm:"index:idx_vm_interface_switch_id"`
	SecurityGroupID *int64

	Model string `gorm:"size:16;not null;default:virtio"`

	// MAC 由控制面分配后固定下来，**不随网卡重建而改变**：来宾系统里可能
	// 已经按 MAC 配过网络（静态 IP、udev 规则），换了 MAC 会让它静默失联。
	MAC *string `gorm:"size:32"`

	// AllowedAddresses 是允许的源地址列表，用于防止 IP 欺骗。
	AllowedAddresses *string `gorm:"type:text"`

	// RateLimitMbps 是限速上限（Mbps）；0 表示不限速。
	RateLimitMbps int `gorm:"not null;default:0"`

	// LastAppliedAt 是最近一次成功下发到节点的时间。
	LastAppliedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMInterface) TableName() string { return "vm_interface" }

// StaticIP 对应 static_ip 表：分配给虚拟机的静态地址。
//
// 它与 `vm.ip_summary` 的区别很重要：ip_summary 是**从节点探测到的实际地址**
// （投影），本表是**控制面期望的分配**。两者不一致时界面应以实际为准——
// 用户需要知道真实情况，而不是我们期望的情况。
type StaticIP struct {
	ID int64 `gorm:"primaryKey"`
	// NodeID 参与 uniq_static_ip_node_ip：**地址在节点内唯一**，
	// 跨节点可以重复（不同宿主机的网段本就独立）。
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_static_ip_node_ip,priority:1"`

	// VMID 为空表示地址已分配但尚未绑定到虚拟机（预留给将来使用）。
	VMID *int64 `gorm:"index:idx_static_ip_vm_id"`

	// InterfaceOrder 对应 VMInterface.Order。
	InterfaceOrder *int

	// 索引是同一地址只能被分配一次的依据：没有它，两次分配会各写一行，
	// 而「这个地址归谁」就没有确定答案了——冲突检测会在受理时通过，
	// 到实际下发时才以两个网卡抢同一个地址的形式暴露。
	IP            string  `gorm:"size:64;not null;uniqueIndex:uniq_static_ip_node_ip,priority:2"`
	MAC           *string `gorm:"size:32"`
	AddressFamily string  `gorm:"size:8;not null;default:ipv4"`

	// IsDHCPReservation 区分这条记录是 DHCP 静态租约，还是来宾内部手工
	// 配置的地址。两者的排查方向完全不同：前者要查 DHCP 服务，后者要进
	// 系统里看配置文件。
	IsDHCPReservation bool `gorm:"not null;default:false"`

	// AppliedAt 是最近一次下发到节点的时间；为空表示尚未生效。
	AppliedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (StaticIP) TableName() string { return "static_ip" }

// 端口转发协议。
const (
	PortProtocolTCP = "tcp"
	PortProtocolUDP = "udp"
)

// PortForward 对应 port_forward 表：把宿主机上的一个端口转发到虚拟机。
//
// 它是**安全敏感资源**：一个误开的转发等于把来宾的服务直接暴露到外部网络，
// 而用户往往在很久之后才发现。因此：
//
//   - 唯一约束是 (node_id, protocol, host_port) —— 端口在节点内独占，
//     两个转发抢同一个端口只会让其中一个静默失效；
//   - 界面应把「允许的来源」放在显眼位置，全放开是一个需要用户明确选择的
//     状态，而不是默认。
type PortForward struct {
	ID     int64  `gorm:"primaryKey"`
	NodeID int64  `gorm:"not null;uniqueIndex:uniq_port_forward_node_proto_port,priority:1;index:idx_port_forward_vm_id,priority:2"`
	VMID   *int64 `gorm:"index:idx_port_forward_vm_id,priority:1"`

	Protocol string `gorm:"size:8;not null;default:tcp;uniqueIndex:uniq_port_forward_node_proto_port,priority:2"`
	HostPort int    `gorm:"not null;uniqueIndex:uniq_port_forward_node_proto_port,priority:3"`

	// TargetIP 与 TargetPort 是转发到虚拟机内部的哪个地址与端口。
	// TargetIP 为空时由节点按虚拟机的实际地址决定。
	TargetIP   *string `gorm:"size:64"`
	TargetPort int     `gorm:"not null"`

	// StaticIPID 指向分配给的静态地址；与 TargetIP 二选一。
	StaticIPID *int64

	// AllowedIPs 是允许访问的来源地址（逗号分隔）。
	// 为空表示**不限制**——这是一个安全性上的重要默认，界面必须显式提示。
	AllowedIPs *string `gorm:"type:text"`
	// AllowedRegions 是允许的来源地区。当前**尚未生效**：它需要节点侧具备
	// GeoIP 能力，而那一层还没实现。保留字段是为了让界面能诚实地显示
	// 「该功能尚未生效」，而不是把它藏起来。
	AllowedRegions *string `gorm:"size:255"`

	// ⚠️ **GORM 陷阱**（同 model/schedule.go 的 Enabled）：`default:true` 时
	// 值为 `false` 会被当作零值省略，数据库填入 `true`。生产代码创建时总是
	// true，不受影响；改动这里之前请先确认这一点。
	Enabled bool `gorm:"not null;default:true"`

	// LastAppliedAt 为空表示规则尚未下发到节点，即**当前并不生效**。
	LastAppliedAt *time.Time

	CreatedBy *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (PortForward) TableName() string { return "port_forward" }
