package node

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// StatsView 是宿主机指标的对外视图（F-6-03）。
//
// 单位写进字段名（MB / Bytes / Seconds）——接口是给人看的，而一个叫 `mem`
// 的字段到底是字节还是 MB，只有写它的人知道。单位错误不会报错，只会让
// 界面把一个数量级错误的数字显示得很正常。
type StatsView struct {
	CPUPercent float64 `json:"cpu_percent"`
	CPUCores   int     `json:"cpu_cores"`

	LoadAvg1  float64 `json:"load_avg_1"`
	LoadAvg5  float64 `json:"load_avg_5"`
	LoadAvg15 float64 `json:"load_avg_15"`

	MemTotalMB int `json:"mem_total_mb"`
	MemUsedMB  int `json:"mem_used_mb"`

	DiskTotalBytes int64 `json:"disk_total_bytes"`
	DiskUsedBytes  int64 `json:"disk_used_bytes"`

	VMCount   int `json:"vm_count"`
	VMRunning int `json:"vm_running"`

	UptimeSeconds int64 `json:"uptime_seconds"`
	// AgentStartedAt 与 UptimeSeconds 分开：agent 重启（升级、崩溃重拉）后
	// 两者会明显不同，而「agent 刚重启过」往往是排查一连串异常的第一条线索。
	AgentStartedAt time.Time `json:"agent_started_at"`

	// At 是这组数据的采集时刻。
	//
	// **必须下发**：指标是瞬时值，界面轮询失败时会继续显示上一组数字，
	// 没有采集时刻的话，用户看到的是一组不知道多久以前的数据。
	At time.Time `json:"at"`
}

// Stats 读取宿主机的实时指标（F-6-03）。
//
// **不入队、不写投影**：与虚拟机的 Stats 同类，是只读探测。指标是瞬时的，
// 存下来只会在下一次读取时给出一个过期的答案——真正需要历史的场景
// （容量趋势、告警）应当由独立采集任务按固定间隔落库。
func (s *Service) Stats(ctx context.Context, id int64) (*StatsView, error) {
	var node model.Node
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&node).Error
	if err != nil {
		return nil, api.NotFound("节点不存在")
	}
	if !node.IsEnrolled() {
		return nil, api.ValidationFailed("节点尚未接入，无法读取指标")
	}

	// 虚拟机数量**由控制面自己统计**，不问节点。
	//
	// 控制面持有虚拟机投影，一次 count 就能给出；让节点报会引入一个新问题：
	// 节点看到的是宿主机上真实存在的域（可能包含面板外手工建的），而面板上
	// 显示的数字必须与列表页一致——两处不一致比数字本身不准更让人困惑。
	var total, running int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND present = ?", id, true).Count(&total).Error; err != nil {
		log.Printf("[node] 统计节点虚拟机数量失败: %v", err)
		return nil, api.Internal()
	}
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND present = ? AND status = ?", id, true, model.VMStatusRunning).
		Count(&running).Error; err != nil {
		log.Printf("[node] 统计节点运行中虚拟机数量失败: %v", err)
		return nil, api.Internal()
	}

	if s.client == nil {
		// 明确说「不支持」而不是给出一堆 0：0% 的 CPU 看起来是「机器很闲」，
		// 而实际是「没读到」——两者对排查的意义完全相反。
		return nil, api.Unavailable("当前部署未启用节点指标采集")
	}

	result, err := s.client.Execute(ctx, agent.Operation{
		Kind: agent.OpNodeStats, NodeID: id,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取指标")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	stats, ok := result.Data[agent.NodeStatsDataKey].(agent.NodeStats)
	if !ok {
		log.Printf("[node] 节点未返回指标 node=%d", id)
		return nil, api.Unavailable("节点未返回运行指标")
	}

	return &StatsView{
		CPUPercent:     stats.CPUPercent,
		CPUCores:       stats.CPUCores,
		LoadAvg1:       stats.LoadAvg1,
		LoadAvg5:       stats.LoadAvg5,
		LoadAvg15:      stats.LoadAvg15,
		MemTotalMB:     stats.MemTotalMB,
		MemUsedMB:      stats.MemUsedMB,
		DiskTotalBytes: stats.DiskTotalBytes,
		DiskUsedBytes:  stats.DiskUsedBytes,
		VMCount:        int(total),
		VMRunning:      int(running),
		UptimeSeconds:  stats.UptimeSeconds,
		AgentStartedAt: stats.AgentStartedAt,
		At:             time.Now(),
	}, nil
}
