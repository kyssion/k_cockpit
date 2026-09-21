package network

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// --- 交换机迁移与重配置 ---

// MigrateSwitchRequest 是一次交换机迁移。
type MigrateSwitchRequest struct {
	// UplinkIf 是目标物理网卡。
	UplinkIf string
	// VlanID 是要一并切换到的 VLAN；为空表示保持当前 VLAN。
	VlanID *int
	// Acknowledge 表示用户已知悉迁移期间网口会短暂中断。
	Acknowledge bool
}

// MigrateSwitch 把交换机迁移到另一块物理网卡。
//
// 它走任务队列而不是同步执行：搬动端口要逐个摘下再挂上，中途的任何一次
// 失败都需要**整批回滚**——而同步接口里没有"回滚到一半"这个状态可返回。
//
// 系统交换机不允许迁移：它是节点的基础网络，动它等于动整台宿主机的连通性，
// 而那不该由一次点击决定。
func (s *Service) MigrateSwitch(
	ctx context.Context, id int64, req MigrateSwitchRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	sw, err := s.loadSwitch(ctx, id)
	if err != nil {
		return nil, err
	}
	if sw.IsSystem {
		return nil, api.ValidationFailed("系统网络不允许迁移：它是节点的基础网络，动它会切断整台宿主机的连通性")
	}
	if strings.TrimSpace(req.UplinkIf) == "" {
		return nil, api.InvalidParameter("必须指定目标物理网卡")
	}
	if req.VlanID != nil && (*req.VlanID < 1 || *req.VlanID > 4094) {
		return nil, api.InvalidParameter("VLAN 需在 1 到 4094 之间")
	}
	if !req.Acknowledge {
		return nil, api.Conflict("迁移期间该网络的网口会短暂中断，确认请勾选")
	}

	spec := map[string]any{"uplink_if": strings.TrimSpace(req.UplinkIf)}
	if req.VlanID != nil {
		spec["vlan_id"] = *req.VlanID
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVPCSwitchChange,
		NodeID:       sw.NodeID,
		ResourceType: "vpc_switch",
		ResourceID:   sw.ID,
		ResourceName: sw.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: switchParams{
			Action: "migrate", NodeID: sw.NodeID, SwitchID: sw.ID,
			BridgeName: sw.BridgeName, Spec: spec,
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ReconfigureSwitch 按当前配置重新下发一遍。
//
// 它对应的是"配置是对的、节点上的状态漂了"（手工改过、升级残留）这一具体
// 场景，因此**不需要任何参数**：参数是"要改成什么"，而重配置改的是"让它与
// 我们记录的一致"。
func (s *Service) ReconfigureSwitch(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	sw, err := s.loadSwitch(ctx, id)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{
		"mode": sw.Mode, "bridge_name": sw.BridgeName,
	}
	if sw.VlanID != nil {
		spec["vlan_id"] = *sw.VlanID
	}
	if sw.CIDR != nil {
		spec["cidr"] = *sw.CIDR
	}
	if sw.GatewayIP != nil {
		spec["gateway_ip"] = *sw.GatewayIP
	}
	if sw.DHCPStart != nil {
		spec["dhcp_start"] = *sw.DHCPStart
	}
	if sw.DHCPEnd != nil {
		spec["dhcp_end"] = *sw.DHCPEnd
	}
	if sw.UplinkIf != nil {
		spec["uplink_if"] = *sw.UplinkIf
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVPCSwitchChange,
		NodeID:       sw.NodeID,
		ResourceType: "vpc_switch",
		ResourceID:   sw.ID,
		ResourceName: sw.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: switchParams{
			Action: "reconfigure", NodeID: sw.NodeID, SwitchID: sw.ID,
			BridgeName: sw.BridgeName, Spec: spec,
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// --- 端口释放 ---

// ReleasePortRequest 释放一个 VPC 端口。
type ReleasePortRequest struct {
	NodeID   int64
	SwitchID *int64
	// PortRef 是端口引用（网桥名 / 虚拟机网口名）。
	PortRef string
	// VMID 便于审计与界面提示"这是哪台机器的网口"。
	VMID *int64
}

// ReleasePort 释放一个 VPC 端口。
//
// 与"删除网卡"的区别：删除是**控制面记录**层面的（这条网口配置没了），
// 释放是**节点资源**层面的（端口占用的内核/OVS 资源要回收）。只删记录会
// 留下一个谁也不用却一直占着的端口，而它最终会以"端口不够用"的形式暴露。
func (s *Service) ReleasePort(
	ctx context.Context, req ReleasePortRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	ref := strings.TrimSpace(req.PortRef)
	if req.NodeID <= 0 || ref == "" {
		return nil, api.InvalidParameter("必须指定节点与端口")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVPCSwitchChange,
		NodeID:       req.NodeID,
		ResourceType: "vpc_port",
		ResourceName: ref,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: switchParams{
			Action: "release_port", NodeID: req.NodeID,
			SwitchID: derefID(req.SwitchID),
			Spec:     map[string]any{"port_ref": ref},
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// --- 计数重置与 IPv6 策略：这两个是**即时动作**，不涉及端口搬动 ---

// CounterResetResult 是计数重置的结果。
type CounterResetResult struct {
	Reset   int    `json:"reset"`
	Message string `json:"message"`
}

// ResetCounters 重置节点上交换机 / 网口的流量计数。
//
// 计数是累计值。换过环境或迁移之后，旧基数会让"这个月用了多少"完全失真；
// 重置让它从零开始，而不是靠人去记一个差值再心算。
func (s *Service) ResetCounters(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*CounterResetResult, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpNetworkCounterReset,
		NodeID: nodeID,
		Target: "counters",
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，计数未重置")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info := decodeCounterReset(result.Data)
	return &CounterResetResult{Reset: info.Reset, Message: firstNonEmpty(info.Message, "计数器已归零")}, nil
}

// IPv6PolicyRequest 是 IPv6 保护策略。
type IPv6PolicyRequest struct {
	NodeID int64
	// Protect 为 true 时启用 IPv6 保护（限制非可信前缀的 IPv6 转发）。
	Protect bool
	// TrustedPrefixes 是可信前缀列表。
	TrustedPrefixes []string
}

// IPv6PolicyResult 是 IPv6 策略下发的结果。
type IPv6PolicyResult struct {
	Applied bool     `json:"applied"`
	Message string   `json:"message"`
	Trusted []string `json:"trusted"`
}

// ApplyIPv6Policy 下发 IPv6 保护策略与可信前缀。
//
// "保护"的语义必须写清：它不是"禁用 IPv6"，而是**只信任列出的前缀**，其余
// IPv6 转发被限制。这两者在界面上一旦混同，用户会以为开启保护等于断网。
func (s *Service) ApplyIPv6Policy(
	ctx context.Context, req IPv6PolicyRequest, v authz.Viewer, operatorName, clientIP string,
) (*IPv6PolicyResult, error) {
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}
	prefixes := []string{}
	for _, p := range req.TrustedPrefixes {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, "/") {
			return nil, api.InvalidParameter("可信前缀必须带掩码长度：" + p)
		}
		if _, _, err := net.ParseCIDR(p); err != nil {
			return nil, api.InvalidParameter("可信前缀不合法：" + p)
		}
		prefixes = append(prefixes, p)
	}
	if req.Protect && len(prefixes) == 0 {
		return nil, api.ValidationFailed(
			"开启 IPv6 保护时必须至少给一个可信前缀：否则等于禁用全部 IPv6 转发")
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpNetworkIPv6Policy,
		NodeID: req.NodeID,
		Target: "ipv6",
		Params: map[string]any{"protect": req.Protect, "trusted_prefixes": prefixes},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，IPv6 策略未下发")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info := decodeIPv6Policy(result.Data)
	out := &IPv6PolicyResult{
		Applied: info.Applied,
		Message: firstNonEmpty(info.Message, "IPv6 策略已下发"),
		Trusted: prefixes,
	}
	if len(info.Trusted) > 0 {
		out.Trusted = info.Trusted
	}
	return out, nil
}

// --- 解码 ---

func decodeCounterReset(data map[string]any) agent.CounterResetInfo {
	return decodeInto[agent.CounterResetInfo](data, agent.CounterResetDataKey)
}

func decodeIPv6Policy(data map[string]any) agent.IPv6PolicyInfo {
	return decodeInto[agent.IPv6PolicyInfo](data, agent.IPv6PolicyDataKey)
}

// decodeInto 从结果里取一个结构。
//
// 走一遍 JSON 是因为同进程拿到的是结构体、跨进程是 map，两种形状都要能读；
// 写两遍解析逻辑迟早会漏掉其中一种。
func decodeInto[T any](data map[string]any, key string) T {
	var zero T
	if data == nil {
		return zero
	}
	raw, ok := data[key]
	if !ok {
		return zero
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return zero
	}
	var out T
	if err := json.Unmarshal(blob, &out); err != nil {
		log.Printf("[network] 解析 %s 失败: %v", key, err)
		return zero
	}
	return out
}

// derefID 取指针值；nil 时为 0（表示"不针对某个交换机"）。
func derefID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// firstNonEmpty 取第一个非空值。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
