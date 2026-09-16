package vm

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
)

// StatsView 是虚拟机运行指标的对外视图。
//
// 每个字段都带单位（MB / Kbps / Seconds）：接口是给人看的，而一个叫
// `mem` 的字段到底是字节还是 MB，只有写它的人知道——单位错误不会报错，
// 只会让界面把一个数量级错误的数字显示得很正常。
type StatsView struct {
	// CPUPercent 是相对全部 vCPU 的占用率（0-100）。
	CPUPercent float64 `json:"cpu_percent"`
	// CPUCores 是配置的核数，供界面显示「2 核 · 37%」。
	CPUCores int `json:"cpu_cores"`

	MemTotalMB int `json:"mem_total_mb"`
	MemUsedMB  int `json:"mem_used_mb"`

	NetRxKbps     float64 `json:"net_rx_kbps"`
	NetTxKbps     float64 `json:"net_tx_kbps"`
	DiskReadKbps  float64 `json:"disk_read_kbps"`
	DiskWriteKbps float64 `json:"disk_write_kbps"`

	// UptimeSeconds 为 0 表示未运行。
	UptimeSeconds int64 `json:"uptime_seconds"`

	// At 是这组数据的采集时刻。
	//
	// **必须下发**：指标是瞬时值，界面轮询失败时会继续显示上一组数字，
	// 没有采集时刻的话，用户看到的是一组不知道多久以前的数据，却以为
	// 它是当前的。界面上据此标注「5 秒前」或「采集失败，以下为 N 分钟前」。
	At time.Time `json:"at"`
}

// Stats 读取一台虚拟机的实时运行指标。
//
// **不入队、不写投影**：它是只读探测，与状态探测同类。指标是瞬时的，
// 存下来只会在下一次读取时给出一个过期的答案——真正需要历史的场景
// （容量趋势、计费）应当由独立的采集任务按固定间隔落库，而不是靠用户
// 打开页面时的顺带写入，那会让记录密度随用户行为起伏。
func (s *Service) Stats(
	ctx context.Context, id int64, v authz.Viewer,
) (*StatsView, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	// 这里**不检查维护模式**：维护模式拦的是写操作（创建、电源、删除），
	// 而查看指标不会改变任何东西。在维护中的节点上禁止看指标，只会让
	// 运维在排查维护对象时缺少最需要的那份数据。
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMStats,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取运行指标")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	stats, ok := result.Data[agent.StatsDataKey].(agent.VMStats)
	if !ok {
		// 节点没按约定返回：给一句可操作的说明，而不是让界面显示一排 0
		// ——0% 的 CPU 看起来是「机器很闲」，而实际是「没读到」。
		log.Printf("[vm] 节点未返回指标 vm=%d node=%d", vm.ID, vm.NodeID)
		return nil, api.Unavailable("节点未返回运行指标")
	}

	return &StatsView{
		CPUPercent:    stats.CPUPercent,
		CPUCores:      vm.VCPU,
		MemTotalMB:    stats.MemTotalMB,
		MemUsedMB:     stats.MemUsedMB,
		NetRxKbps:     stats.NetRxKbps,
		NetTxKbps:     stats.NetTxKbps,
		DiskReadKbps:  stats.DiskReadKbps,
		DiskWriteKbps: stats.DiskWriteKbps,
		UptimeSeconds: stats.UptimeSeconds,
		At:            time.Now(),
	}, nil
}

// ConsoleFrame 抓取一帧控制台画面（Hero 的控制台预览卡）。
//
// 返回**原始字节**，由接口层决定编码。这里不转 base64：那会让调用方以为
// 画面本来就是字符串，而它实际是二进制——将来换 JPEG 或改成流式时，
// 这个假设会变成障碍。
func (s *Service) ConsoleFrame(
	ctx context.Context, id int64, v authz.Viewer,
) (*agent.ConsoleFrame, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	// 没有显示设备的虚拟机直接拒绝，而不是让节点去抓一个抓不到的画面。
	//
	// 判据与控制台入口一致（has_console）：两处若各判各的，就会出现
	// 「详情页显示着预览卡、点进去却没有控制台」这种自相矛盾的界面。
	if !vm.HasConsole() {
		return nil, api.ValidationFailed("该虚拟机没有显示设备，未开启控制台")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMConsoleFrame,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法获取控制台画面")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	frame, ok := result.Data[agent.FrameDataKey].(agent.ConsoleFrame)
	if !ok || len(frame.Data) == 0 {
		return nil, api.Unavailable("节点未返回控制台画面")
	}
	if frame.MIME == "" {
		frame.MIME = "image/png"
	}
	return &frame, nil
}
