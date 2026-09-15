// Package network 实现网络后端抽象与能力降级（F-4-01）。
//
// 本包的核心不是「怎么配网桥」，而是**怎么把配不了这件事说清楚**：
// 缺一个依赖就笼统报「网络配置失败」，用户只能靠猜；而告诉他缺什么、
// 影响哪些功能、装什么能修，他就能自己解决（R-011）。
//
// 能力探测结果区分**三态**（Q-003）：「可用」「不可用（附原因与修复）」
// 「未知（探测失败）」。用布尔值会把「探测失败」与「确认缺失」混为一谈，
// 从而误判降级或误判可用。
package network

import (
	"context"
	"errors"
	"log"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// CapabilityState 是能力的三态。
type CapabilityState string

// 能力状态。
const (
	// StateAvailable 确认可用。
	StateAvailable CapabilityState = "available"
	// StateUnavailable 确认缺失，附原因与修复方式。
	StateUnavailable CapabilityState = "unavailable"
	// StateUnknown 探测失败，无法判断。
	//
	// 它与「缺失」的差别是实的：缺失要装东西，未知要重试。把后者显示成
	// 前者会让用户去装一个其实已经装好的包。
	StateUnknown CapabilityState = "unknown"
)

// 后端模式。
const (
	// ModeBasic Linux 网桥 + dnsmasq + NAT。零额外依赖，M2 的默认模式（Q-002）。
	ModeBasic = "basic"
	// ModeOVS Open vSwitch，M3 起启用。
	ModeOVS = "ovs"
)

// Capability 是一项网络能力的状态。
type Capability struct {
	Key   string          `json:"key"`
	Label string          `json:"label"`
	State CapabilityState `json:"state"`
	// Required 表示该能力是基础网络的必需项；缺失即降级。
	Required bool `json:"required"`
	// Reason 说明缺的是什么。
	Reason string `json:"reason,omitempty"`
	// Fix 是可执行的修复方式。给命令而不是「请联系管理员」——
	// 能打开这个页面的人就是要执行命令的人。
	Fix string `json:"fix,omitempty"`
	// AffectedFeatures 列出受影响的功能，让用户能判断「要不要现在处理」。
	AffectedFeatures []string `json:"affected_features,omitempty"`
}

// StatusView 是网络后端状态。
type StatusView struct {
	NodeID    int64  `json:"node_id"`
	Mode      string `json:"mode"`
	ModeLabel string `json:"mode_label"`
	// Degraded 表示必需能力有缺失，功能受限。
	Degraded bool `json:"degraded"`
	// ProbeFailed 表示本次探测失败，所有能力状态为「未知」。
	ProbeFailed  bool         `json:"probe_failed"`
	ProbeMessage string       `json:"probe_message,omitempty"`
	Capabilities []Capability `json:"capabilities"`
}

// SwitchView 是网络的对外视图。
type SwitchView struct {
	ID         int64  `json:"id"`
	NodeID     int64  `json:"node_id"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	BridgeName string `json:"bridge_name"`
	CIDR       string `json:"cidr,omitempty"`
	GatewayIP  string `json:"gateway_ip,omitempty"`
	DHCPStart  string `json:"dhcp_start,omitempty"`
	DHCPEnd    string `json:"dhcp_end,omitempty"`
	UplinkIf   string `json:"uplink_if,omitempty"`
	IsSystem   bool   `json:"is_system"`
	Status     string `json:"status"`
}

// capabilityMeta 是能力清单的**静态部分**。
//
// 「缺了怎么办」写在控制面而不是由 agent 上报：agent 只需回答「有没有」，
// 而给用户看什么提示是产品决策。让每个节点的实现各自决定文案，会让同一个
// 缺失在不同节点上给出不同的解释。
type capabilityMeta struct {
	Key      string
	Label    string
	Required bool
	Reason   string
	Fix      string
	Affected []string
}

var capabilityCatalog = []capabilityMeta{
	{
		Key:      agent.CapabilityBridgeBasic,
		Label:    "Linux 网桥",
		Required: true,
		Reason:   "未检测到网桥工具（bridge / ip）",
		Fix:      "apt install bridge-utils iproute2   # 或：yum install bridge-utils iproute",
		Affected: []string{"创建虚拟机", "虚拟机之间的网络连通"},
	},
	{
		Key:      agent.CapabilityDHCP,
		Label:    "DHCP / DNS 服务",
		Required: true,
		Reason:   "未检测到 dnsmasq",
		Fix:      "apt install dnsmasq   # 或：yum install dnsmasq",
		Affected: []string{"虚拟机自动获取 IP", "虚拟机域名解析"},
	},
	{
		Key:      agent.CapabilityNAT,
		Label:    "NAT 出网",
		Required: true,
		Reason:   "未检测到 iptables / nftables",
		Fix:      "apt install iptables   # 或：yum install iptables",
		Affected: []string{"虚拟机访问外网"},
	},
	{
		Key:   agent.CapabilityOVS,
		Label: "Open vSwitch",
		// 非必需：缺它只失去进阶能力，基础网络照常工作（R-004）。
		Required: false,
		Reason:   "未检测到 Open vSwitch",
		Fix:      "apt install openvswitch-switch   # 或：yum install openvswitch",
		Affected: []string{"VPC 交换机", "网络隔离策略", "带宽限制"},
	},
}

// Service 提供网络领域查询。
type Service struct {
	db    *gorm.DB
	agent agent.Client
}

// NewService 构造网络服务。
func NewService(db *gorm.DB, client agent.Client) *Service {
	return &Service{db: db, agent: client}
}

// Status 返回节点的网络后端状态与能力清单。
func (s *Service) Status(ctx context.Context, nodeID int64) (*StatusView, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}

	backend, probeErr := s.probe(ctx, nodeID)

	view := &StatusView{
		NodeID:    nodeID,
		Mode:      ModeBasic,
		ModeLabel: "基础模式（Linux 网桥）",
	}
	if backend != nil && backend.Mode == ModeOVS {
		view.Mode = ModeOVS
		view.ModeLabel = "Open vSwitch"
	}

	// 探测失败时所有能力是「未知」，而不是「缺失」（Q-003）。
	if probeErr != nil {
		view.ProbeFailed = true
		view.ProbeMessage = probeErr.Error()
	}

	has := map[string]bool{}
	if backend != nil {
		for _, c := range backend.Capabilities {
			has[c] = true
		}
	}

	for _, meta := range capabilityCatalog {
		cap := Capability{
			Key:      meta.Key,
			Label:    meta.Label,
			Required: meta.Required,
		}

		switch {
		case probeErr != nil:
			cap.State = StateUnknown
		case has[meta.Key]:
			cap.State = StateAvailable
		default:
			cap.State = StateUnavailable
			cap.Reason = meta.Reason
			cap.Fix = meta.Fix
			cap.AffectedFeatures = meta.Affected
			// 只有**必需**能力缺失才算降级。OVS 缺失让视图更丰富，
			// 但不影响「虚拟机能不能联网」这件事（R-004）。
			if meta.Required {
				view.Degraded = true
			}
		}
		view.Capabilities = append(view.Capabilities, cap)
	}

	return view, nil
}

// Networks 返回节点的可用网络。
//
// 首次查询时会**建立系统基础网络**（幂等，R-008）。它没有放在注册流程里，
// 是因为系统基础网络是节点的**固有属性**而非业务数据：任何节点都必然有它
// （R-006），因此按需建立比「要求某个流程必须先跑过」更不容易出错——
// 少一个「如果当初注册流程没走完就永远没有网络」的隐式依赖。
func (s *Service) Networks(ctx context.Context, nodeID int64) ([]SwitchView, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	if err := s.ensureSystemNetwork(ctx, nodeID); err != nil {
		return nil, err
	}

	var switches []model.VpcSwitch
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND deleted_at IS NULL", nodeID).
		Order("is_system DESC, id").
		Find(&switches).Error
	if err != nil {
		log.Printf("[network] 查询网络失败: %v", err)
		return nil, api.Internal()
	}

	views := make([]SwitchView, 0, len(switches))
	for i := range switches {
		views = append(views, toSwitchView(&switches[i]))
	}
	return views, nil
}

// probe 探测节点的网络后端。
//
// 返回的 error 表示**探测本身失败**（节点不可达），与「探测成功但能力
// 缺失」是两回事——调用方据此区分「未知」与「不可用」。
func (s *Service) probe(ctx context.Context, nodeID int64) (*agent.NetworkBackend, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{Kind: agent.OpNodeNetwork, NodeID: nodeID})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法探测网络能力")
	}
	if !result.Success {
		return nil, api.Unavailable("节点未返回网络能力信息")
	}

	backend, ok := result.Data[agent.NetworkKey].(agent.NetworkBackend)
	if !ok {
		return nil, api.Unavailable("节点返回的网络能力信息格式不正确")
	}
	return &backend, nil
}

// ensureNode 校验节点存在。
func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	err := s.db.WithContext(ctx).Select("id").Where("id = ?", nodeID).First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[network] 查询节点失败: %v", err)
		return api.Internal()
	}
	return nil
}

// ensureSystemNetwork 保证系统基础网络存在。
//
// **幂等**（R-008）：重复执行不产生重复记录。用「先查后建」而不是靠唯一
// 索引兜底，是为了让并发调用也走在同一条路径上——索引冲突虽然也能防重复，
// 但它会以错误的形式暴露，而这里期望的是「静默确保存在」。
func (s *Service) ensureSystemNetwork(ctx context.Context, nodeID int64) error {
	var count int64
	err := s.db.WithContext(ctx).Model(&model.VpcSwitch{}).
		Where("node_id = ? AND is_system = ? AND deleted_at IS NULL", nodeID, true).
		Count(&count).Error
	if err != nil {
		log.Printf("[network] 查询系统网络失败: %v", err)
		return api.Internal()
	}
	if count > 0 {
		return nil
	}

	bridgeName := "kbr0"
	mode := model.NetworkModeNAT
	sw := model.VpcSwitch{
		NodeID:     nodeID,
		Name:       model.NetworkDefaultName,
		Mode:       mode,
		BridgeName: bridgeName,
		IsSystem:   true,
		Status:     model.NetworkActive,
	}
	err = s.db.WithContext(ctx).Create(&sw).Error
	if err != nil {
		// 并发下的唯一索引冲突不算失败：另一个请求刚建好了它。
		if isDuplicateKey(err) {
			return nil
		}
		log.Printf("[network] 建立系统基础网络失败 node=%d: %v", nodeID, err)
		return api.Internal()
	}

	log.Printf("[network] 已建立系统基础网络 node=%d name=%s", nodeID, sw.Name)
	return nil
}

func toSwitchView(sw *model.VpcSwitch) SwitchView {
	return SwitchView{
		ID:         sw.ID,
		NodeID:     sw.NodeID,
		Name:       sw.Name,
		Mode:       sw.Mode,
		BridgeName: sw.BridgeName,
		CIDR:       derefStr(sw.CIDR),
		GatewayIP:  derefStr(sw.GatewayIP),
		DHCPStart:  derefStr(sw.DHCPStart),
		DHCPEnd:    derefStr(sw.DHCPEnd),
		UplinkIf:   derefStr(sw.UplinkIf),
		IsSystem:   sw.IsSystem,
		Status:     sw.Status,
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
