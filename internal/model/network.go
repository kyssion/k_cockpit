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
	ID      int64 `gorm:"primaryKey"`
	NodeID  int64 `gorm:"not null"`
	OwnerID *int64
	Name    string `gorm:"size:64;not null"`
	Mode    string `gorm:"size:16;not null;default:empty"`
	// BridgeName 是宿主机上的网桥名。
	BridgeName string `gorm:"size:64;not null"`
	VlanID     *int   `gorm:"column:vlan_id"`
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
