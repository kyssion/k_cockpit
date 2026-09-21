package dashboard

import (
	"context"
	"encoding/json"
	"log"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/hosttuning"
	"k_cockpit/internal/model"
)

// TuningProvider 提供宿主机调优状态（KSM / zRAM / 嵌套虚拟化）。
//
// 声明成接口而不是直接依赖 hosttuning 包：工作台只需要"读一次状态"，
// 而 hosttuning 的状态读取本身依赖 agent。用接口把这块换出去之后，
// 工作台的测试不需要构造一整套调优服务。
type TuningProvider interface {
	Get(ctx context.Context, nodeID int64) (*hosttuning.View, error)
}

// SetTuning 装配调优状态来源。不调用时 KSM / zRAM 区段不显示——
// 那比显示一个永远为空的卡片更诚实。
func (s *Service) SetTuning(p TuningProvider) { s.tuning = p }

// HostDetail 是某个节点的宿主细节。
type HostDetail struct {
	NodeID   int64               `json:"node_id"`
	NodeName string              `json:"node_name"`
	Tuning   *hosttuning.View    `json:"tuning,omitempty"`
	Hardware *agent.HostHardware `json:"hardware,omitempty"`
	NetStats *agent.HostNetStats `json:"netstats,omitempty"`
}

// HostDetailOf 读取某节点的宿主细节（管理员）。
//
// 三块数据**各自独立失败**：节点可能支持硬件探测却不支持网络统计（或
// 反过来）。让其中一块的失败把整页变成错误，用户就看不到另外两块——
// 而那两块正是他打开这一页想看的东西。
//
// 租户不可用：宿主机的内存插了几条、网桥转发了多少，与租户无关，
// 且属于节点层面的信息。
func (s *Service) HostDetail(ctx context.Context, nodeID int64, v authz.Viewer) (*HostDetail, error) {
	if !v.IsAdmin {
		return nil, api.NotFound("宿主机细节仅管理员可查看")
	}
	if nodeID <= 0 {
		return nil, api.InvalidParameter("必须指定节点")
	}

	// 节点名一并带出：界面上要写清"这是哪台宿主机"，否则在多节点部署里
	// 这些数字没有主语。
	var n struct {
		ID   int64
		Name string
	}
	if err := s.db.WithContext(ctx).Model(&model.Node{}).
		Where("id = ?", nodeID).Scan(&n).Error; err != nil || n.ID == 0 {
		return nil, api.NotFound("节点不存在")
	}

	out := &HostDetail{NodeID: nodeID, NodeName: n.Name}

	if s.tuning != nil {
		if view, err := s.tuning.Get(ctx, nodeID); err == nil {
			out.Tuning = view
		} else {
			log.Printf("[dashboard] 读取调优状态失败 node=%d: %v", nodeID, err)
		}
	}

	if s.agent != nil {
		out.Hardware = probeHardware(ctx, s.agent, nodeID, n.Name)
		out.NetStats = probeNetStats(ctx, s.agent, nodeID, n.Name)
	}
	return out, nil
}

// probeHardware 读取硬件构成；失败时返回带说明的空结构。
func probeHardware(ctx context.Context, cli agent.Client, nodeID int64, name string) *agent.HostHardware {
	result, err := cli.Execute(ctx, agent.Operation{
		Kind: agent.OpHostHardware, NodeID: nodeID, Target: name,
	})
	if err != nil || result == nil || !result.Success || result.Data == nil {
		return &agent.HostHardware{Unavailable: "节点未提供硬件信息"}
	}
	raw, ok := result.Data[agent.HostHardwareDataKey]
	if !ok {
		return &agent.HostHardware{Unavailable: "节点未提供硬件信息"}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return &agent.HostHardware{Unavailable: "硬件信息无法解析"}
	}
	var hw agent.HostHardware
	if err := json.Unmarshal(blob, &hw); err != nil {
		return &agent.HostHardware{Unavailable: "硬件信息无法解析"}
	}
	return &hw
}

// probeNetStats 读取网络统计；失败时返回带说明的空结构。
func probeNetStats(ctx context.Context, cli agent.Client, nodeID int64, name string) *agent.HostNetStats {
	result, err := cli.Execute(ctx, agent.Operation{
		Kind: agent.OpHostNetStats, NodeID: nodeID, Target: name,
	})
	if err != nil || result == nil || !result.Success || result.Data == nil {
		return &agent.HostNetStats{Unavailable: "节点未提供网络统计"}
	}
	raw, ok := result.Data[agent.HostNetStatsDataKey]
	if !ok {
		return &agent.HostNetStats{Unavailable: "节点未提供网络统计"}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return &agent.HostNetStats{Unavailable: "网络统计无法解析"}
	}
	var st agent.HostNetStats
	if err := json.Unmarshal(blob, &st); err != nil {
		return &agent.HostNetStats{Unavailable: "网络统计无法解析"}
	}
	return &st
}
