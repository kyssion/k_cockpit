package agent

// OpHostStats 读取宿主机的运行指标（F-8-01）。
//
// 与 OpVMStats 同类：只读探测，不入队、不写投影。区别只在于它的目标是
// **宿主机本身**而不是某台虚拟机。
const OpHostStats OpKind = "host.stats"

// HostStatsDataKey 是结果中承载指标的键。
const HostStatsDataKey = "host_stats"

// HostStats 是节点上报的宿主机指标。
//
// 四个流量/IO 字段是**自开机以来的累计字节数**，不是速率。存累计值是因为
// 速率要靠两次采样相减得到，而如果某次采样丢了（节点不可达），那一整段就
// 没有速率可用；存累计值则只是区间变长，总量仍然对。
type HostStats struct {
	CPUPercent float64
	MemUsedMB  int64
	MemTotalMB int64
	SwapUsedMB int64

	// Load1/5/15 与 CPU 百分比并存：后者是瞬时快照，前者反映「这段时间有
	// 多少活等着」。排查「机器很卡但 CPU 不高」时看的是后者。
	Load1  float64
	Load5  float64
	Load15 float64

	NetInBytes     int64
	NetOutBytes    int64
	DiskReadBytes  int64
	DiskWriteBytes int64

	// Devices 是每块设备的明细（JSON 文本），供 F-8-05 的硬件详情使用。
	Devices string
	// UptimeSeconds 是宿主机已运行的时间。
	UptimeSeconds int64
}
