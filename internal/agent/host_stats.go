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
// HostDeviceStat 是一块**物理设备**在本次采样里的累计量。
//
// 单独成结构而不是只报总量：宿主机上的网卡与磁盘各有若干块，而"整体流量
// 突然涨了"之后第一个问题永远是"是哪一块涨的"。只有总量永远回答不了这个
// 问题，而按设备存一份的代价只是每次采样多几十字节。
type HostDeviceStat struct {
	// Name 是设备名（eth0 / sda…），它是唯一标识。
	Name string
	// Kind 取值 net / disk。
	Kind string
	// ReadBytes 对网卡是**入向**字节，对磁盘是**读**字节。
	ReadBytes int64
	// WriteBytes 对网卡是**出向**字节，对磁盘是**写**字节。
	WriteBytes int64
}

type HostStats struct {
	CPUPercent float64
	// CPUCores 是逻辑核心数。
	//
	// 与 CPUPercent **必须一起上报**：百分比是"占了多少"，核数是"总共
	// 有多少"，只有后者才能把「已承诺给虚拟机的 vCPU」换算成占比——那是
	// 工作台"理论最大量"的分母。此前它只在按需探测的 NodeStats 里有，
	// 于是任何"按历史算承诺占比"的需求都只能回到逐节点实时探测。
	CPUCores   int
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

	// Devices 是每块设备的明细（JSON 文本），元素为 HostDeviceStat 数组。
	Devices string
	// UptimeSeconds 是宿主机已运行的时间。
	UptimeSeconds int64
}
