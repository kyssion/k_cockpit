package agent

// OpMigrateAssess 是迁移线路评估操作：节点间测一次可用带宽。
// 参数里带 target_node_id（对端），节点据此选择测速路径。
const OpMigrateAssess OpKind = "vm.migrate.assess"

// MigrateAssess 是迁移预检的线路评估（G-35）。
//
// 它回答的是停机迁移里「停多久」的量化依据：两节点之间的**实测带宽**。
// 没有它，预检只能按「假设千兆」给量级估计——而那条公式对万兆环境
// 会把停机时间高估十倍，对半截的千兆（实际 300 Mbps）低估三倍。
type MigrateAssessInfo struct {
	// BandwidthMbps 是测得的可用带宽（Mbps）。控制面用它与磁盘容量换算
	// 预计复制时长。
	BandwidthMbps int64 `json:"bandwidth_mbps"`
	// DirtyRateMBps 是虚拟机当前的脏页速率（MB/s），仅热迁移评估时有值；
	// 0 表示未测得或不适用（停机迁移不需要它）。
	//
	// 脏页速率与带宽的比值决定热迁移能否收敛：脏页产生得比传输快，
	// 迁移永远追不上（F-2-15 的阈值判定依据）。
	DirtyRateMBps int64 `json:"dirty_rate_mbps,omitempty"`
	// Source 说明数字的来源：speedtest（实测）还是 estimate（按链路规格
	// 估算）。两个来源的置信度不同，界面上要能区分。
	Source string `json:"source"`
	// Message 是节点给的补充说明（测速耗时、丢包率等），可空。
	Message string `json:"message,omitempty"`
}

// AssessSource 是评估来源的取值。
const (
	// AssessSourceSpeedtest 实测：节点间真实传输了一小段数据。
	AssessSourceSpeedtest = "speedtest"
	// AssessSourceEstimate 估算：按网卡协商速率或链路规格推算。
	AssessSourceEstimate = "estimate"
)

// MigrateAssessDataKey 是 Execute 结果里评估数据的键。
const MigrateAssessDataKey = "migrate_assess"
