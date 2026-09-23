package agent

// 宿主机的**细节**（F-6-03 的补充）：这些东西只在用户主动查看时才需要，
// 因此不与 host.stats 的周期采样混在一起。
const (
	// OpHostHardware 读取 CPU 与内存条的硬件构成。
	//
	// 为什么单独一个操作：周期采样里只需要"用了百分之几"，而这里要的是
	// 插槽、核心、线程与每条内存的占用情况——它们的来源完全不同
	// （dmidecode / libvirt capabilities），采样频率也差两个数量级。
	OpHostHardware OpKind = "host.hardware"

	// OpHostNetStats 读取网络统计。
	//
	// 统计的是**规则与计数**，不是实时流量：实时流量由 host.stats 采样
	// 承担，而"现在有多少条 DNAT 规则""交换机转发了多少"是排查连通性
	// 时才会问的问题。
	OpHostNetStats OpKind = "host.netstats"
)

// HostHardwareDataKey 是硬件信息结果的键。
const HostHardwareDataKey = "host_hardware"

// MemSlot 是一个内存插槽。
//
// json tag 不可省：这些类型会被直接嵌进 HTTP 响应（API.md §1.2 要求响应
// 字段统一用下划线），缺 tag 时序列化出的是 Go 字段名。
type MemSlot struct {
	// Index 是插槽编号，从 1 开始——界面上显示"插槽 2"比显示索引 1
	// 更接近机箱上贴的那个号。
	Index     int    `json:"index"`
	SizeMB    int64  `json:"size_mb"`
	Populated bool   `json:"populated"`
	Label     string `json:"label"`
}

// HostHardware 是宿主机的硬件构成。
//
// Unavailable 非空表示**探测不到**，而不是"机器没有内存"。这两者在界面上
// 必须分开：空列表会被读成"这台机器没有内存条"，而实际只是节点没实现
// 这个操作。
type HostHardware struct {
	CPUModel       string `json:"cpu_model"`
	Sockets        int    `json:"sockets"`
	CoresPerSocket int    `json:"cores_per_socket"`
	ThreadsPerCore int    `json:"threads_per_core"`
	// CorePercent 是每个逻辑核心的占用百分比，顺序即核心编号。
	CorePercent []float64 `json:"core_percent"`
	MemSlots    []MemSlot `json:"mem_slots"`
	Unavailable string    `json:"unavailable"`
}

// HostNetStatsDataKey 是网络统计结果的键。
const HostNetStatsDataKey = "host_netstats"

// BridgeStat 是一个网桥的收发统计。
type BridgeStat struct {
	Name      string `json:"name"`
	RxBytes   int64  `json:"rx_bytes"`
	TxBytes   int64  `json:"tx_bytes"`
	RxPackets int64  `json:"rx_packets"`
	TxPackets int64  `json:"tx_packets"`
}

// HostNetStats 是宿主机上的网络规则与计数。
type HostNetStats struct {
	// NATRules 是 NAT 网关规则条数。
	NATRules int `json:"nat_rules"`
	// SwitchIngressBytes / SwitchEgressBytes 是虚拟交换机的入/出口字节数。
	SwitchIngressBytes int64 `json:"switch_ingress_bytes"`
	SwitchEgressBytes  int64 `json:"switch_egress_bytes"`
	// DNATRules 是 iptables DNAT 规则条数。
	DNATRules   int          `json:"dnat_rules"`
	Bridges     []BridgeStat `json:"bridges"`
	Unavailable string       `json:"unavailable"`
}
