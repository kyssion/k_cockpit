package agent

import "time"

// OpNodeStats 读取宿主机的运行指标（F-6-03）。
//
// 与 OpVMStats 同类：**只读探测，不入队、不写投影**。指标是瞬时的，存下来
// 只会在下一次读取时给出一个过期的答案。
const OpNodeStats OpKind = "node.stats"

// NodeStatsDataKey 是 OpNodeStats 结果中承载指标的键。
const NodeStatsDataKey = "node_stats"

// NodeStats 是节点上报的宿主机指标。
//
// 单位一律写进字段名（MB / Bytes / Percent）：一个叫 `mem` 的字段到底是
// 字节、KB 还是 MB，只有写它的人知道，而读它的人只能去翻实现——单位错误
// 不会报错，只会让界面把一个数量级错误的数字显示得很正常。
type NodeStats struct {
	// CPUPercent 是整机 CPU 占用率（0-100），相对所有核心。
	CPUPercent float64
	// CPUCores 是逻辑核心数，供界面显示「8 核 · 37%」。
	CPUCores int
	// LoadAvg1 / 5 / 15 是一分钟、五分钟、十五分钟的平均负载。
	//
	// 与 CPU 占用率**一起看**才有意义：占用率是瞬时的，负载是趋势。
	// 一台占用率只有 20% 但负载持续在核数以上的机器，说明有大量等待 IO
	// 的进程——那是光看占用率看不出来的问题。
	LoadAvg1  float64
	LoadAvg5  float64
	LoadAvg15 float64

	MemTotalMB int
	MemUsedMB  int

	// DiskTotalBytes / DiskUsedBytes 是该节点上**所有存储池**的合计。
	//
	// 用字节而不是 GB：宿主机磁盘常常不是整数 GB，凑整会让「还剩多少」
	// 在临界时看起来比实际多。
	DiskTotalBytes int64
	DiskUsedBytes  int64

	// UptimeSeconds 是宿主机自开机以来的运行时间。
	UptimeSeconds int64
	// AgentStartedAt 是 agent 进程的启动时刻。
	//
	// 与宿主机运行时间分开：agent 重启（升级、崩溃重拉）后这两者会明显不同，
	// 而「agent 刚重启过」往往是排查一连串异常的第一条线索。
	AgentStartedAt time.Time
}
