package network

import (
	"context"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/settings"
)

// --- 全局带宽总限（G-44）---

// GlobalBandwidthResult 是全局带宽下发的结果。
type GlobalBandwidthResult struct {
	Applied   bool   `json:"applied"`
	Mbps      int    `json:"mbps"`
	BurstMbps int    `json:"burst_mbps"`
	Message   string `json:"message"`
}

// ApplyGlobalBandwidth 把设置里的全局带宽总限下发到节点。
//
// 总限的值存在系统设置里（network.global_bandwidth_mbps），这里是**动作**
// 而不是配置：改设置只改控制面的值，节点上的整形规则要等这次下发才变。
// 分开的原因与"重载规则"一致——设置可以被批量回滚，而节点上的状态只能
// 靠一次明确的动作去对齐。
func (s *Service) ApplyGlobalBandwidth(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*GlobalBandwidthResult, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点")
	}

	mbps := s.settings.Int(settings.KeyNetworkGlobalBandwidth, 0)
	burst := s.settings.Int(settings.KeyNetworkGlobalBurst, 0)
	if burst > 0 && mbps <= 0 {
		return nil, api.ValidationFailed(
			"未设置全局带宽总限时不能只填突发峰值：突发是总限之上的短暂超额，没有总限它没有意义")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpNetworkGlobalBandwidth,
		NodeID: nodeID,
		Target: "uplink",
		Params: map[string]any{"mbps": mbps, "burst_mbps": burst},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，总限未下发")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info := decodeGlobalBandwidth(result.Data)
	return &GlobalBandwidthResult{
		Applied:   info.Applied,
		Mbps:      info.Mbps,
		BurstMbps: info.BurstMbps,
		Message:   firstNonEmpty(info.Message, "全局带宽总限已下发"),
	}, nil
}

func decodeGlobalBandwidth(data map[string]any) agent.GlobalBandwidthInfo {
	return decodeInto[agent.GlobalBandwidthInfo](data, agent.GlobalBandwidthDataKey)
}
