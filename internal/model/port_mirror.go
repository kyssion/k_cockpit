package model

import "time"

// 镜像方向。
const (
	MirrorBoth    = "both"
	MirrorIngress = "ingress"
	MirrorEgress  = "egress"
)

// PortMirror 对应 port_mirror 表：端口镜像（F-4-09）。
//
// 把若干来源接口的流量复制到若干**空交换机**上，供抓包与分析使用。
//
// 它的失败模式与防火墙不同，值得单独说清楚，因为它决定了下面这个字段：
//
//	防火墙配错 → **你自己进不去面板**（管理连接被挡）
//	端口镜像配错 → **可能打垮宿主机网络**（环路导致流量自我放大、
//	              或把生产流量灌进一个不该有流量的地方）
//
// 第二种更糟：它不只是「你进不去」，而是「这台机器上的业务也一起完了」。
// 而这两种情况下都有一个共同点——**控制面是通过网络下发指令的，网络断了
// 它就什么都做不了**。
//
// 所以「启用前建立自动回滚看门狗」这件事**必须由节点自己执行**：节点在
// 启用镜像时记住「如果在 T 时刻前没收到确认，就自己撤销」，而那个判断
// 不能依赖任何一次网络往返。下面这个字段是控制面对这件事的**记录**，
// 不是它的执行者——执行者在节点侧（见 agent.OpPortMirrorEnable 的说明）。
type PortMirror struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;index:idx_port_mirror_node_id"`

	// Name 供识别。可以为空，但界面上会建议填——多个镜像规则并存时，
	// 一个没有名字的规则只能靠 id 分辨。
	Name *string `gorm:"size:64"`

	// SourcePorts / TargetSwitches 都是**换行分隔的标识列表**。
	//
	// 「多来源 → 多目标」是 f-4-09 明确要求的。用一列存列表而不是建关联表，
	// 是因为这两个列表**只在整条规则被整体替换时才会变**——没有「单独给
	// 这条规则加一个来源」这种操作，因此不需要行级的增删。
	SourcePorts    *string `gorm:"type:text"`
	TargetSwitches *string `gorm:"type:text"`

	Direction string `gorm:"size:8;not null;default:both"`

	// VlanPreserve 决定复制出去的帧是否保留 VLAN 标签。
	//
	// 默认 false（剥掉标签）：抓包时看到的往往是**去掉了 VLAN 的帧**，
	// 因为多数分析工具对带标签的帧处理得不好。要分析 VLAN 本身的行为时
	// 才需要打开它。
	VlanPreserve bool `gorm:"not null;default:false"`

	Enabled bool `gorm:"not null;default:false"`

	// WatchdogUntil 非空表示**看门狗仍在计时**。
	//
	// 语义要分清，它与 Enabled 是两件事：
	//
	//	enabled=true,  watchdog_until 非空 → 已生效，等待用户确认「保持」
	//	enabled=true,  watchdog_until 为空 → 已生效，用户已确认
	//	enabled=false, watchdog_until 为空 → 未启用
	//
	// 因此「清掉 watchdog_until」这个动作**同时意味着两件事**：要么用户
	// 确认了保持，要么镜像已经被回滚了。区分它们要靠 enabled——这也是为什么
	// 不能只用一个字段表达状态。
	WatchdogUntil *time.Time

	// LastAppliedAt 是最近一次成功下发到节点的时刻。
	LastAppliedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (PortMirror) TableName() string { return "port_mirror" }

// ValidMirrorDirection 报告方向取值是否合法。
func ValidMirrorDirection(d string) bool {
	switch d {
	case MirrorBoth, MirrorIngress, MirrorEgress:
		return true
	}
	return false
}

// IsAwaitingConfirm 报告该镜像是否正处于「等待用户确认保持」的窗口里。
//
// 这是一个**真实且短暂**的状态：启用的那一刻就开始计时，用户看着网络没
// 出问题就点确认。界面必须把它显眼地标出来——窗口一旦过去而用户没确认，
// 镜像会被节点自动撤销，而那时他会以为是系统出了问题。
func (m *PortMirror) IsAwaitingConfirm() bool {
	return m.Enabled && m.WatchdogUntil != nil
}
