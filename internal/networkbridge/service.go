// Package networkbridge 实现网络底座的状态、能力降级与自愈（F-4-01 / F-4-13）。
//
// 本包几乎所有的设计都由规格里的一句话决定：
//
//	**网络配置失败不阻断主流程，但必须可见、可诊断。**
//
// 它听起来理所当然，但很容易做错：把「探测网络状态」写成一个必须在页面
// 渲染前成功完成的步骤，那么网络一坏，用户就看到白屏或 500——而他本来
// 正是来这里看网络出了什么问题的。
//
// 因此本包的约定是：
//
//   - **探测失败是数据，不是异常**。Overview 在任何一步失败时都不返回
//     error，而是把失败写进对应字段（ProbeOK=false、BridgesError=...），
//     让界面能把「不知道」如实显示出来。
//   - **降级要说明影响了什么**。缺 OVS 不是静默换个后端就完事——必须指
//     出哪些既有网络依赖它，否则用户会在某个功能不可用时完全找不到原因。
package networkbridge

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// 入桥自动回滚窗口的边界。
//
// 下限 60 秒：太短的窗口会让操作者来不及回去确认——而他此刻很可能正在
// 换一个网络重新连上来。上限 30 分钟：再长的话，一个已经断掉管理通道的
// 桥会一直挂着，而自动回滚本来就是用来兜住这种事的。
const (
	minUplinkWatchdogSeconds     = 60
	maxUplinkWatchdogSeconds     = 1800
	defaultUplinkWatchdogSeconds = 300
)

// Service 提供网络底座能力。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	audit *audit.Recorder
	now   func() time.Time
}

// NewService 构造服务。
func NewService(db *gorm.DB, client agent.Client, recorder *audit.Recorder) *Service {
	return &Service{db: db, agent: client, audit: recorder, now: time.Now}
}

// CapabilityView 是节点网络能力的对外视图。
type CapabilityView struct {
	OVSAvailable     bool              `json:"ovs_available"`
	OVSVersion       string            `json:"ovs_version,omitempty"`
	KernelModules    []string          `json:"kernel_modules"`
	UplinkCandidates []UplinkCandidate `json:"uplink_candidates"`
}

// UplinkCandidate 是一个上行口候选。
type UplinkCandidate struct {
	Name string `json:"name"`
	Up   bool   `json:"up"`
	// HasIP 是**最重要的一个字段**：有 IP 的口很可能是管理口，把它加进
	// 桥里就是把自己关在门外。界面据此把这类候选标红并要求额外确认。
	HasIP bool   `json:"has_ip"`
	Speed string `json:"speed,omitempty"`
}

// BridgeView 是一张桥的对外视图。
type BridgeView struct {
	ID      int64  `json:"id"`
	NodeID  int64  `json:"node_id"`
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Mode    string `json:"mode"`

	UplinkIf    string `json:"uplink_if,omitempty"`
	CIDR        string `json:"cidr,omitempty"`
	GatewayIP   string `json:"gateway_ip,omitempty"`
	DHCPEnabled bool   `json:"dhcp_enabled"`
	DHCPStart   string `json:"dhcp_start,omitempty"`
	DHCPEnd     string `json:"dhcp_end,omitempty"`
	VlanID      *int   `json:"vlan_id,omitempty"`

	IsSystem bool   `json:"is_system"`
	Status   string `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Remark   string `json:"remark,omitempty"`

	// NeedsOVS 表示该桥依赖 Open vSwitch。
	//
	// OVS 缺失时界面据此**具体指出**哪些网络受影响，而不是笼统地说一句
	// 「已降级」——后者对用户没有任何可操作性。
	NeedsOVS bool `json:"needs_ovs"`
	// AwaitingUplinkConfirm 为 true 表示正处于入桥的自动回滚窗口里。
	AwaitingUplinkConfirm bool   `json:"awaiting_uplink_confirm"`
	UplinkWatchdogUntil   string `json:"uplink_watchdog_until,omitempty"`
	// UplinkSecondsLeft 由**服务端**算好下发（客户端时钟不可信）。
	UplinkSecondsLeft int    `json:"uplink_seconds_left"`
	CreatedAt         string `json:"created_at"`
}

// Overview 是网络底座的完整状态（F-4-13 的展示部分）。
//
// **任何一个字段都可能带着失败信息，而整体仍然返回 200**。这是「不阻断
// 主流程」的具体实现：界面永远能打开，并且能如实显示「我不知道」，而不是
// 白屏。
type Overview struct {
	NodeID int64 `json:"node_id"`

	// ProbeOK 为 false 时下面的 Capability **不可信**，界面应显示「状态未知」
	// 而不是把它当成「什么都没有」。
	ProbeOK    bool   `json:"probe_ok"`
	ProbeError string `json:"probe_error,omitempty"`

	Capability *CapabilityView `json:"capability,omitempty"`

	// Degraded 为 true 表示运行在基础模式上。
	Degraded bool `json:"degraded"`
	// DegradedReasons 说明降级**影响了什么**。
	//
	// f-4-01 明确要求「不能在能力缺失时静默失败，必须明确提示并说明了影响
	// 哪些功能」。因此这里列的是具体的桥与具体的功能，而不是一句
	// 「OVS 不可用」。
	DegradedReasons []string `json:"degraded_reasons,omitempty"`

	Bridges      []BridgeView `json:"bridges"`
	BridgesError string       `json:"bridges_error,omitempty"`

	// Warnings 是跨字段的提醒（如「有桥的自动回滚窗口即将到期」）。
	Warnings []string `json:"warnings,omitempty"`
}

// Overview 汇总网络状态。
//
// **它几乎不会返回 error**：除了「节点根本不存在」这类硬错误，其余失败
// 都写进对应的字段。这是有意的——见包注释。
func (s *Service) Overview(ctx context.Context, nodeID int64) (*Overview, error) {
	out := &Overview{NodeID: nodeID, Bridges: []BridgeView{}}

	if err := s.ensureNode(ctx, nodeID); err != nil {
		// 节点不存在是**硬错误**：连不上节点与「节点上的网络有问题」是
		// 两件事，前者没有可展示的内容。
		return nil, err
	}

	// 1) 能力探测。失败只写字段。
	out.Capability, out.ProbeOK, out.ProbeError = s.probe(ctx, nodeID)

	// 2) 桥列表。数据库查询失败同样不阻断——界面至少要能看到「网络状态
	//    暂时读不出来」，而不是整页打不开。
	bridges, err := s.listBridges(ctx, nodeID)
	if err != nil {
		out.BridgesError = "网络列表读取失败，请稍后重试"
	} else {
		out.Bridges = bridges
	}

	// 3) 降级判定。
	s.applyDegradation(out)

	// 4) 跨字段提醒。
	for i := range out.Bridges {
		b := &out.Bridges[i]
		if b.AwaitingUplinkConfirm && b.UplinkSecondsLeft > 0 && b.UplinkSecondsLeft <= 60 {
			out.Warnings = append(out.Warnings,
				"「"+b.Name+"」的物理口入桥将在 "+strconv.Itoa(b.UplinkSecondsLeft)+
					" 秒后自动回滚——若网络正常请立即确认保持")
		}
	}

	return out, nil
}

// probe 探测节点网络能力。
func (s *Service) probe(ctx context.Context, nodeID int64) (*CapabilityView, bool, string) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNetworkProbe, NodeID: nodeID, Target: "network",
	})
	if err != nil {
		// 只说「不可达」，不把底层错误原样抛给用户——那对判断没有帮助。
		return nil, false, "节点不可达，网络能力未知"
	}
	if !result.Success {
		return nil, false, "节点未能完成网络探测：" + result.Message
	}

	cap, ok := result.Data[agent.NetworkCapabilityKey].(agent.NetworkCapability)
	if !ok {
		return nil, false, "节点返回的探测结果无法解析"
	}

	view := &CapabilityView{
		OVSAvailable:  cap.OVSAvailable,
		OVSVersion:    cap.OVSVersion,
		KernelModules: cap.KernelModules,
	}
	for _, c := range cap.UplinkCandidates {
		view.UplinkCandidates = append(view.UplinkCandidates, UplinkCandidate{
			Name: c.Name, Up: c.Up, HasIP: c.HasIP, Speed: c.Speed,
		})
	}
	return view, true, ""
}

// applyDegradation 判定降级并**说明影响的落点**。
func (s *Service) applyDegradation(out *Overview) {
	if !out.ProbeOK || out.Capability == nil {
		// 探测失败时**不宣称降级**：「不知道」与「没有 OVS」是两件事，
		// 而把它们混为一谈会让用户去修一个并不存在的问题。
		return
	}
	if out.Capability.OVSAvailable {
		return
	}

	out.Degraded = true
	out.DegradedReasons = append(out.DegradedReasons,
		"节点上没有可用的 Open vSwitch，已降级到 Linux 网桥（基础模式）。"+
			"虚拟机创建、默认网络与端口转发不受影响。")

	// 具体列出受影响的桥——一句「已降级」对用户没有任何可操作性。
	var affected []string
	for i := range out.Bridges {
		if out.Bridges[i].NeedsOVS {
			affected = append(affected, out.Bridges[i].Name)
		}
	}
	if len(affected) > 0 {
		out.DegradedReasons = append(out.DegradedReasons,
			"以下网络建立在 OVS 上，降级后无法正常转发："+strings.Join(affected, "、")+
				"——需要在该节点安装 Open vSwitch 后执行「修复」")
	} else {
		out.DegradedReasons = append(out.DegradedReasons,
			"当前没有网络依赖 OVS，因此降级暂未影响任何既有网络。")
	}
}

// ListBridges 返回节点上的桥。
func (s *Service) ListBridges(ctx context.Context, nodeID int64) ([]BridgeView, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	return s.listBridges(ctx, nodeID)
}

func (s *Service) listBridges(ctx context.Context, nodeID int64) ([]BridgeView, error) {
	var rows []model.NetworkBridge
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("is_system DESC, id ASC").Find(&rows).Error; err != nil {
		log.Printf("[networkbridge] 查询桥失败: %v", err)
		return nil, api.Internal()
	}
	views := make([]BridgeView, 0, len(rows))
	for i := range rows {
		views = append(views, s.toBridgeView(&rows[i]))
	}
	return views, nil
}

// BridgeRequest 是创建或更新一张桥的请求。
type BridgeRequest struct {
	NodeID      int64
	Name        string
	Backend     string
	Mode        string
	CIDR        string
	GatewayIP   string
	DHCPStart   string
	DHCPEnd     string
	DHCPEnabled bool
	VlanID      *int
	Remark      string
}

// CreateBridge 新建一张桥（**不接物理口**）。
//
// 创建与入桥分开：创建只是建一个虚拟网络，而入桥会动宿主机的连通性。
// 合成一步会让「先建好、确认没问题、再接物理口」这个次序无法表达。
func (s *Service) CreateBridge(
	ctx context.Context, req BridgeRequest, v authz.Viewer, operatorName, clientIP string,
) (*BridgeView, error) {
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}
	n, err := normalizeBridge(req, false)
	if err != nil {
		return nil, err
	}

	row := model.NetworkBridge{
		NodeID: req.NodeID, Name: n.name, Backend: n.backend, Mode: n.mode,
		CIDR: n.cidr, GatewayIP: n.gatewayIP, DHCPStart: n.dhcpStart, DHCPEnd: n.dhcpEnd,
		DHCPEnabled: req.DHCPEnabled, VlanID: req.VlanID,
		Status: model.BridgePending, IsSystem: false,
	}
	if req.Remark != "" {
		row.Remark = &req.Remark
	}

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该节点下已有同名网络")
		}
		log.Printf("[networkbridge] 创建失败: %v", err)
		return nil, api.Internal()
	}

	// 下发。失败**不回滚记录**，而是把状态标成 error 并带上原因。
	//
	// 原因就是那句「不阻断主流程、但必须可见可诊断」：记录留着，用户才能
	// 看到「这条网络建了但没起来」，然后去「修复」；删掉记录只会让那次
	// 失败凭空消失。
	applied, msg := s.applyBridge(ctx, &row)
	if !applied {
		s.markError(ctx, row.ID, msg)
	} else {
		s.markActive(ctx, row.ID)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "network_bridge", ResourceID: row.ID,
		ResourceName: n.name, Action: "network_bridge.create",
		Params:  map[string]any{"mode": n.mode, "backend": n.backend, "applied": applied},
		Success: applied, Error: msg, ClientIP: clientIP,
	})

	row.Status = model.BridgeActive
	if !applied {
		row.Status = model.BridgeError
		row.Detail = &msg
	}
	view := s.toBridgeView(&row)
	return &view, nil
}

// DeleteBridge 删除一张桥。
//
// **系统预置网络不可删**：新建虚拟机默认接的就是它，删掉等于让「新建」从
// 可用变成不可用，而用户不会立刻把这两件事联系起来。
func (s *Service) DeleteBridge(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	row, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if row.IsSystem {
		return api.ValidationFailed(
			"「" + row.Name + "」是系统预置网络，不可删除；" +
				"新建虚拟机会默认接入它")
	}
	if row.IsAwaitingUplinkConfirm() {
		return api.ValidationFailed("该网络的物理口入桥窗口尚未结束，请先确认保持或等待自动回滚")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNetworkBridgeDelete, NodeID: row.NodeID, Target: row.Name,
	})
	if err != nil {
		s.markError(ctx, row.ID, "节点不可达")
		return api.Unavailable("节点不可达，网络未删除")
	}
	if !result.Success {
		s.markError(ctx, row.ID, result.Message)
		return api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Delete(&model.NetworkBridge{}, row.ID).Error; err != nil {
		log.Printf("[networkbridge] 删除失败: %v", err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "network_bridge", ResourceID: row.ID,
		ResourceName: row.Name, Action: "network_bridge.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// AttachUplinkResult 是入桥结果。
type AttachUplinkResult struct {
	Applied         bool        `json:"applied"`
	WatchdogSeconds int         `json:"watchdog_seconds"`
	Warnings        []string    `json:"warnings,omitempty"`
	Bridge          *BridgeView `json:"bridge,omitempty"`
}

// AttachUplink 把物理口加入桥（F-4-01：必须可回滚且有显式确认）。
//
// 这是网络模块里最危险的一个操作：把一个物理口加到桥上会**重置它的 IP
// 配置**——如果那恰好是管理口，操作者当场失联，而那时他已经没有任何界面
// 路径可以改回来。
//
// 两道防线，缺一不可：
//
//   - **显式确认**防的是「没想清楚就点了」；
//   - **自动回滚窗口**防的是「确认过之后才发现不行」——口加进去了、网络
//     断了、面板打不开了。
//
// 第二道的执行者必须在**节点侧**：控制面是通过网络下发指令的，而这个操作
// 的结果恰恰可能是网络断掉。一个依赖网络的保险，在它最需要起作用的时候
// 一定不在。
func (s *Service) AttachUplink(
	ctx context.Context, id int64, uplinkIf string, watchdogSeconds int, acknowledge bool,
	v authz.Viewer, operatorName, clientIP string,
) (*AttachUplinkResult, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.IsAwaitingUplinkConfirm() {
		return nil, api.ValidationFailed("该网络已有一个待确认的入桥操作")
	}

	iface := strings.TrimSpace(uplinkIf)
	if iface == "" {
		return nil, api.InvalidParameter("必须指定要入桥的物理口")
	}
	seconds := watchdogSeconds
	if seconds <= 0 {
		seconds = defaultUplinkWatchdogSeconds
	}
	if seconds < minUplinkWatchdogSeconds || seconds > maxUplinkWatchdogSeconds {
		return nil, api.InvalidParameter(
			"自动回滚窗口必须在 " + strconv.Itoa(minUplinkWatchdogSeconds) + " 秒到 " +
				strconv.Itoa(maxUplinkWatchdogSeconds) + " 秒之间")
	}

	// 警告由**节点探测到的真实候选**给出，而不是让控制面猜。
	warnings := s.uplinkWarnings(ctx, row, iface)
	if len(warnings) > 0 && !acknowledge {
		return &AttachUplinkResult{Warnings: warnings, Applied: false}, nil
	}

	until := s.now().Add(time.Duration(seconds) * time.Second)
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpNetworkUplinkAttach,
		NodeID: row.NodeID,
		Target: row.Name,
		Params: map[string]any{
			"bridge":           row.Name,
			"uplink_if":        iface,
			"watchdog_seconds": seconds,
		},
	})
	if err != nil {
		s.markError(ctx, row.ID, "节点不可达")
		return nil, api.Unavailable("节点不可达，物理口未入桥")
	}
	if !result.Success {
		s.markError(ctx, row.ID, result.Message)
		return nil, api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Model(&model.NetworkBridge{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"uplink_if":             iface,
			"uplink_watchdog_until": until,
			"status":                model.BridgeActive,
			"detail":                nil,
		}).Error; err != nil {
		log.Printf("[networkbridge] 回写入桥状态失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "network_bridge", ResourceID: row.ID,
		ResourceName: row.Name, Action: "network_bridge.uplink_attach",
		Params: map[string]any{
			"uplink_if": iface, "watchdog_seconds": seconds,
			"acknowledged_warnings": warnings,
		},
		Success: true, ClientIP: clientIP,
	})

	row.UplinkIf = &iface
	row.UplinkWatchdogUntil = &until
	row.Status = model.BridgeActive
	row.Detail = nil
	view := s.toBridgeView(row)
	return &AttachUplinkResult{
		Applied: true, WatchdogSeconds: seconds, Warnings: warnings, Bridge: &view,
	}, nil
}

// uplinkWarnings 给出入桥前的提醒。
//
// 最有价值的一条是**「这个口上有 IP」**：那通常意味着它是管理口，把它加进
// 桥里就是把自己关在门外。这一点控制面自己判断不了（它看不到宿主机的
// 网卡配置），因此要靠探测结果里的 HasIP。
func (s *Service) uplinkWarnings(ctx context.Context, row *model.NetworkBridge, iface string) []string {
	var warnings []string

	cap, ok, _ := s.probe(ctx, row.NodeID)
	if !ok || cap == nil {
		// 探测不到时**照常给出通用提醒**而不是放行：宁可让用户多看一句话，
		// 也不要在一个说不清风险的情况下让他点下去。
		warnings = append(warnings,
			"无法探测节点网络：不能确认 "+iface+" 上是否已有 IP 配置。"+
				"若它承载管理流量，入桥后会立刻失联——"+
				"窗口期内不要关闭本页面，到点会自动回滚")
		return warnings
	}

	var found *UplinkCandidate
	for i := range cap.UplinkCandidates {
		if cap.UplinkCandidates[i].Name == iface {
			found = &cap.UplinkCandidates[i]
			break
		}
	}
	if found == nil {
		// 候选列表是节点给的，不在里面的口多半不存在或不可用。
		warnings = append(warnings,
			"节点上没有找到可用的物理口「"+iface+"」——入桥很可能失败")
		return warnings
	}
	if found.HasIP {
		warnings = append(warnings,
			"「"+iface+"」上已经配置了 IP。如果它承载管理流量，"+
				"入桥会**重置这个口的 IP 配置**，你将立刻失去连接。")
	}
	if !found.Up {
		warnings = append(warnings,
			"「"+iface+"」当前链路未接通（DOWN）：入桥不会报错，但流量走不通")
	}
	if !cap.OVSAvailable && row.Backend == model.BackendOVS {
		warnings = append(warnings,
			"该网络建立在 OVS 上，而节点当前没有可用的 Open vSwitch——入桥无法生效")
	}
	return warnings
}

// ConfirmUplink 确认保持，取消自动回滚。
func (s *Service) ConfirmUplink(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*BridgeView, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !row.IsAwaitingUplinkConfirm() {
		return nil, api.ValidationFailed("该网络当前没有待确认的入桥窗口")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNetworkUplinkConfirm, NodeID: row.NodeID, Target: row.Name,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法取消自动回滚")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Model(&model.NetworkBridge{}).
		Where("id = ?", row.ID).Update("uplink_watchdog_until", nil).Error; err != nil {
		log.Printf("[networkbridge] 取消入桥窗口失败: %v", err)
		return nil, api.Internal()
	}
	row.UplinkWatchdogUntil = nil

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "network_bridge", ResourceID: row.ID,
		ResourceName: row.Name, Action: "network_bridge.uplink_confirm",
		Success: true, ClientIP: clientIP,
	})
	view := s.toBridgeView(row)
	return &view, nil
}

// DetachUplink 把物理口从桥里摘出来。
//
// **不做确认、不做预检**：一个已经切断管理通道的桥，用户需要的是一键摘掉。
func (s *Service) DetachUplink(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*BridgeView, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !row.IsUplinked() {
		view := s.toBridgeView(row)
		return &view, nil
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNetworkUplinkDetach, NodeID: row.NodeID,
		Target: row.Name, Params: map[string]any{"uplink_if": *row.UplinkIf},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，物理口未摘出")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Model(&model.NetworkBridge{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{"uplink_if": nil, "uplink_watchdog_until": nil}).Error; err != nil {
		log.Printf("[networkbridge] 摘出入口失败: %v", err)
		return nil, api.Internal()
	}
	row.UplinkIf, row.UplinkWatchdogUntil = nil, nil

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "network_bridge", ResourceID: row.ID,
		ResourceName: row.Name, Action: "network_bridge.uplink_detach",
		Success: true, ClientIP: clientIP,
	})
	view := s.toBridgeView(row)
	return &view, nil
}

// RepairResult 是修复结果。
type RepairResult struct {
	// Fixed 是本次修好的条目。
	Fixed []string `json:"fixed"`
	// Remaining 是修复之后仍然存在的问题。
	//
	// 与 Fixed 分开是必要的：一次部分成功的修复，只报告「修好了什么」
	// 会让用户以为没事了；只报告「还有问题」又看不出这次操作做了什么。
	Remaining []string `json:"remaining"`
	// ProbeOK 为 false 时下面的结论**不可信**。
	ProbeOK    bool   `json:"probe_ok"`
	ProbeError string `json:"probe_error,omitempty"`
	Message    string `json:"message"`
}

// Repair 尝试把网络恢复到期望状态（F-4-13 的「修复」入口）。
//
// 它是一个**幂等的收敛动作**，而不是「重试上一次失败的操作」：重试一件
// 已经失败的事通常不会得到不同结果，而收敛会先看清现状再补差异。
//
// 修复本身失败时**不返回 error**，而是把它写进 Remaining——这与整个模块
// 「失败是数据」的约定一致：用户点「修复」多半是因为网络已经坏了，此时
// 再收到一个 500 对他没有任何帮助。
func (s *Service) Repair(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*RepairResult, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}

	out := &RepairResult{Fixed: []string{}, Remaining: []string{}}
	cap, probeOK, probeErr := s.probe(ctx, nodeID)
	out.ProbeOK, out.ProbeError = probeOK, probeErr

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNetworkRepair, NodeID: nodeID, Target: "network",
	})
	if err != nil {
		out.Message = "节点不可达，无法执行修复"
		out.Remaining = append(out.Remaining, "节点不可达")
	} else if !result.Success {
		out.Message = result.Message
		out.Remaining = append(out.Remaining, result.Message)
	} else if info, ok := result.Data[agent.NetworkRepairKey].(agent.NetworkRepairInfo); ok {
		out.Fixed = append(out.Fixed, info.Fixed...)
		out.Remaining = append(out.Remaining, info.Remaining...)
		out.Message = info.Message
	} else {
		out.Message = "修复已完成"
	}

	// 修复之后按探测结果刷新每张桥的状态：一条网络可能没被修好，而另一条
	// 本来就正常——把它们一起标成 error 会让用户去查一个并不存在的问题。
	if probeOK && cap != nil {
		s.refreshBridgeStatus(ctx, nodeID, cap)
	}
	if !probeOK {
		out.Remaining = append(out.Remaining,
			"网络能力探测未成功，因此无法确认修复后的实际状态")
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "network", Action: "network.repair",
		Params:  map[string]any{"fixed": out.Fixed, "remaining": out.Remaining},
		Success: len(out.Remaining) == 0, ClientIP: clientIP,
	})
	return out, nil
}

// Reconcile 把「入桥窗口已到期」的桥在控制面这边对齐。
//
// 与端口镜像同理：**回滚的执行者是节点**（它到点会自己把口摘出来），这里
// 只让控制面的记录追上节点已经做完的事。两者必须分开——如果只有控制面这
// 一侧扫，那么在一次面板宕机或网络中断期间做的入桥就永远不会被回滚。
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	res := s.db.WithContext(ctx).Model(&model.NetworkBridge{}).
		Where("uplink_watchdog_until IS NOT NULL AND uplink_watchdog_until < ?", s.now()).
		Updates(map[string]any{"uplink_if": nil, "uplink_watchdog_until": nil})
	if res.Error != nil {
		log.Printf("[networkbridge] 对齐到期入桥窗口失败: %v", res.Error)
		return 0, api.Internal()
	}
	if res.RowsAffected > 0 {
		log.Printf("[networkbridge] %d 张桥的入桥窗口已到期，节点应已自动摘出入口", res.RowsAffected)
	}
	return int(res.RowsAffected), nil
}

// --- 内部 ---

type bridgeNormalized struct {
	name      string
	backend   string
	mode      string
	cidr      *string
	gatewayIP *string
	dhcpStart *string
	dhcpEnd   *string
}

func normalizeBridge(req BridgeRequest, isSystem bool) (bridgeNormalized, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return bridgeNormalized{}, api.InvalidParameter("必须填写网络名称")
	}
	if len(name) > 64 {
		return bridgeNormalized{}, api.InvalidParameter("网络名称最长 64 字")
	}

	backend := req.Backend
	if backend == "" {
		backend = model.BackendBridge
	}
	if !model.ValidBridgeBackend(backend) {
		return bridgeNormalized{}, api.InvalidParameter("后端必须是 bridge 或 ovs")
	}

	mode := req.Mode
	if mode == "" {
		mode = model.BridgeModeNAT
	}
	if !model.ValidBridgeMode(mode) {
		return bridgeNormalized{}, api.InvalidParameter("模式必须是 nat / routed / isolated")
	}

	n := bridgeNormalized{name: name, backend: backend, mode: mode}
	n.cidr = optString(req.CIDR)
	n.gatewayIP = optString(req.GatewayIP)
	n.dhcpStart = optString(req.DHCPStart)
	n.dhcpEnd = optString(req.DHCPEnd)

	// 空交换机不该配 NAT 相关的字段：它不接外网，配了也只是没人用，
	// 而界面上多出来的那些填框会让人以为它是可配的。
	if mode == model.BridgeModeIsolated && req.DHCPEnabled {
		return bridgeNormalized{}, api.ValidationFailed(
			"空交换机不接外网，不能启用内置 DHCP")
	}
	if req.DHCPEnabled {
		if n.dhcpStart == nil || n.dhcpEnd == nil {
			return bridgeNormalized{}, api.InvalidParameter(
				"启用内置 DHCP 时必须给出地址池的起止地址")
		}
		if n.cidr == nil || n.gatewayIP == nil {
			return bridgeNormalized{}, api.InvalidParameter(
				"启用内置 DHCP 时必须给出网段与网关地址——" +
					"DHCP 要告诉来客网关是谁，没有它就出不了网")
		}
	}
	_ = isSystem
	return n, nil
}

func (s *Service) applyBridge(ctx context.Context, row *model.NetworkBridge) (bool, string) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNetworkBridgeApply, NodeID: row.NodeID, Target: row.Name,
		Params: map[string]any{
			"name": row.Name, "backend": row.Backend, "mode": row.Mode,
			"cidr": deref(row.CIDR), "gateway_ip": deref(row.GatewayIP),
			"dhcp_enabled": row.DHCPEnabled, "vlan_id": row.VlanID,
		},
	})
	if err != nil {
		return false, "节点不可达"
	}
	if !result.Success {
		return false, result.Message
	}
	return true, ""
}

// refreshBridgeStatus 按探测结果刷新每张桥的状态。
func (s *Service) refreshBridgeStatus(ctx context.Context, nodeID int64, cap *CapabilityView) {
	var rows []model.NetworkBridge
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
		log.Printf("[networkbridge] 刷新状态时查询失败: %v", err)
		return
	}
	for i := range rows {
		b := &rows[i]
		// 依赖 OVS 而 OVS 不在：这条网络确实不可用。
		if b.NeedsOVS() && !cap.OVSAvailable {
			s.markError(ctx, b.ID,
				"节点上没有可用的 Open vSwitch，该网络无法正常转发")
			continue
		}
		// 其它情况**不动状态**：探测成功不代表这张桥就好了，反过来也不代表
		// 它坏了。把不确定的判成某一种，会让用户去查一个并不存在的问题。
		if b.Status == model.BridgePending {
			s.markActive(ctx, b.ID)
		}
	}
}

func (s *Service) markActive(ctx context.Context, id int64) {
	if err := s.db.WithContext(ctx).Model(&model.NetworkBridge{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": model.BridgeActive, "detail": nil}).Error; err != nil {
		log.Printf("[networkbridge] 标记 active 失败: %v", err)
	}
}

func (s *Service) markError(ctx context.Context, id int64, detail string) {
	if err := s.db.WithContext(ctx).Model(&model.NetworkBridge{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": model.BridgeError, "detail": detail}).Error; err != nil {
		log.Printf("[networkbridge] 标记 error 失败: %v", err)
	}
}

func (s *Service) load(ctx context.Context, id int64) (*model.NetworkBridge, error) {
	var row model.NetworkBridge
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("网络不存在")
		}
		log.Printf("[networkbridge] 查询失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	if err := s.db.WithContext(ctx).
		Select("id", "enroll_state").Where("id = ?", nodeID).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		log.Printf("[networkbridge] 查询节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("节点尚未接入")
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func (s *Service) toBridgeView(row *model.NetworkBridge) BridgeView {
	view := BridgeView{
		ID: row.ID, NodeID: row.NodeID, Name: row.Name,
		Backend: row.Backend, Mode: row.Mode,
		DHCPEnabled: row.DHCPEnabled, VlanID: row.VlanID,
		IsSystem: row.IsSystem, Status: row.Status,
		NeedsOVS:              row.NeedsOVS(),
		AwaitingUplinkConfirm: row.IsAwaitingUplinkConfirm(),
		CreatedAt:             row.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	view.UplinkIf = deref(row.UplinkIf)
	view.CIDR = deref(row.CIDR)
	view.GatewayIP = deref(row.GatewayIP)
	view.DHCPStart = deref(row.DHCPStart)
	view.DHCPEnd = deref(row.DHCPEnd)
	view.Detail = deref(row.Detail)
	view.Remark = deref(row.Remark)

	if row.UplinkWatchdogUntil != nil {
		view.UplinkWatchdogUntil = row.UplinkWatchdogUntil.Format("2006-01-02T15:04:05Z07:00")
		left := int(row.UplinkWatchdogUntil.Sub(s.now()).Seconds())
		if left < 0 {
			left = 0
		}
		view.UplinkSecondsLeft = left
	}
	return view
}

func optString(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
