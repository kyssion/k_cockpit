package model

import "time"

// HostStatsRecord 对应 host_stats_record 表：宿主机的采样明细（F-8-01）。
//
// **它由采样器按固定间隔写入**，而不是"用户打开页面时顺便采一次"。
//
// 这一点值得说清楚，因为把两者混起来是很自然的一步：页面已经在轮询指标了
// （Hero 的实时卡），顺手写一条记录看起来是"免费的"。但那样得到的是一份
// **密度由点击行为决定的伪历史**——有人看的时候一秒一条，没人看的时候
// 一条都没有；用它算"过去一周的负载"会得到与真实情况毫无关系的结果。
//
// 采集有它自己的节奏，与谁在看无关。
type HostStatsRecord struct {
	ID     int64     `gorm:"primaryKey"`
	NodeID int64     `gorm:"not null;index:idx_host_stats_node_at,priority:1"`
	At     time.Time `gorm:"not null;index:idx_host_stats_node_at,priority:2"`

	CPUPercent float64 `gorm:"column:cpu_percent;not null;default:0"`
	MemUsedMB  int64   `gorm:"not null;default:0"`
	MemTotalMB int64   `gorm:"not null;default:0"`
	SwapUsedMB int64   `gorm:"not null;default:0"`

	// Load1/5/15 是三个时间尺度的平均负载。
	//
	// 与单独的 CPU 百分比**并存而不是二选一**：CPU 百分比是瞬时值，一次
	// 采样只是那一瞬间的快照；而负载均值反映的是"这段时间有多少活等着"。
	// 排查"机器很卡但 CPU 不高"这类问题时，看的是后者。
	Load1  float64 `gorm:"not null;default:0"`
	Load5  float64 `gorm:"not null;default:0"`
	Load15 float64 `gorm:"not null;default:0"`

	// 以下四个是**累计值**（自开机以来的字节数），不是速率。
	//
	// 存累计值而不是速率：速率要靠"两次采样相减"得到，而如果某次采样丢了
	// （节点不可达），那一整段就没有速率可用。存累计值则只是那一段的区间
	// 变长，总量仍然对。
	NetInBytes     int64 `gorm:"not null;default:0"`
	NetOutBytes    int64 `gorm:"not null;default:0"`
	DiskReadBytes  int64 `gorm:"not null;default:0"`
	DiskWriteBytes int64 `gorm:"not null;default:0"`

	DeviceStats   *string `gorm:"type:text"`
	UptimeSeconds int64   `gorm:"not null;default:0"`
	CreatedAt     time.Time
}

// TableName 固定表名。
func (HostStatsRecord) TableName() string { return "host_stats_record" }

// VMStatsRecord 对应 vm_stats_record 表：虚拟机的采样明细（F-8-02）。
type VMStatsRecord struct {
	ID     int64     `gorm:"primaryKey"`
	VMID   int64     `gorm:"not null;index:idx_vm_stats_vm_at,priority:1"`
	NodeID int64     `gorm:"not null;index:idx_vm_stats_node_at,priority:1"`
	At     time.Time `gorm:"not null;index:idx_vm_stats_vm_at,priority:2;index:idx_vm_stats_node_at,priority:2"`

	CPUPercent float64 `gorm:"column:cpu_percent;not null;default:0"`
	MemUsedMB  int64   `gorm:"not null;default:0"`
	MemPercent float64 `gorm:"not null;default:0"`

	NetInBytes     int64 `gorm:"not null;default:0"`
	NetOutBytes    int64 `gorm:"not null;default:0"`
	DiskReadBytes  int64 `gorm:"not null;default:0"`
	DiskWriteBytes int64 `gorm:"not null;default:0"`
	DiskIOPS       int   `gorm:"column:disk_iops;not null;default:0"`

	UptimeSeconds int64 `gorm:"not null;default:0"`
	CreatedAt     time.Time
}

// TableName 固定表名。
func (VMStatsRecord) TableName() string { return "vm_stats_record" }

// VMRuntimeDaily 对应 vm_runtime_daily 表：虚拟机的**按天运行时长**。
//
// 它不是采样表，而是**累计表**：每次采样如果这台机器在运行，就给当天加上
// 一个采样间隔。它支撑两件事：
//
//   - F-8-06 的「运行时长与超限处置」；
//   - F-1-08 配额里的「运行时长」维度。
//
// **按天分桶而不是存一条累计总数**：配额通常按月算，而"这个月用了多少小时"
// 需要能按时间段查——只存一个总数就只能看到有史以来的累计，无法回答"上个月
// 用了多少"。分桶之后，按日期范围求和即可。
type VMRuntimeDaily struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"not null;uniqueIndex:uniq_vm_runtime_vm_date,priority:1"`
	NodeID int64 `gorm:"not null"`
	// OwnerID 冗余自虚拟机，用于按用户聚合。
	//
	// 与 vm 连表也能算，但配额查询是高频的，而"某用户在某段时间的运行时长"
	// 是这个表最常见的用法——冗余一列让它可以单表完成。代价是机器转手时
	// 要一并更新（那是一次管理操作，代价可接受）。
	OwnerID *int64 `gorm:"index:idx_vm_runtime_owner_date,priority:1"`

	Date time.Time `gorm:"type:date;not null;uniqueIndex:uniq_vm_runtime_vm_date,priority:2;index:idx_vm_runtime_owner_date,priority:2"`
	// Seconds 是当天的累计运行秒数。
	Seconds int `gorm:"not null;default:0"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMRuntimeDaily) TableName() string { return "vm_runtime_daily" }

// 流量统计的作用域（traffic_stat_daily.scope_type）。
const (
	// TrafficScopeVM 按虚拟机统计。
	TrafficScopeVM = "vm"
	// TrafficScopeNode 按节点统计。
	TrafficScopeNode = "node"
)

// TrafficStatDaily 对应 traffic_stat_daily 表：按天的流量累计（F-4-10）。
//
// 与 running time 同一套思路：采样时把**区间增量**累加到当天。它支撑
// 「月流量」这个配额维度，以及超限之后按 `limited_at` 做处置。
type TrafficStatDaily struct {
	ID int64 `gorm:"primaryKey"`
	// ScopeType 取值 vm / node。
	//
	// 一份表承载两种作用域而不是两张表：它们的形状完全一样，而拆开之后
	// "某个用户这个月用了多少流量"要在两个地方各写一遍。
	ScopeType string `gorm:"size:8;not null;uniqueIndex:uniq_traffic_stat_scope_date,priority:1"`
	ScopeID   int64  `gorm:"not null;uniqueIndex:uniq_traffic_stat_scope_date,priority:2"`
	OwnerID   *int64 `gorm:"index:idx_traffic_stat_owner_date,priority:1"`

	Date     time.Time `gorm:"type:date;not null;uniqueIndex:uniq_traffic_stat_scope_date,priority:3;index:idx_traffic_stat_owner_date,priority:2"`
	BytesIn  int64     `gorm:"not null;default:0"`
	BytesOut int64     `gorm:"not null;default:0"`

	// LimitedAt 是**因为超限而被处置**的时刻；为空表示当天没有触发过处置。
	//
	// 记录它而不是只记一个布尔：处置是"限速"还是"断网"、什么时候做的，
	// 在事后解释"为什么这台机器那天晚上变慢了"时都要用到。
	LimitedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (TrafficStatDaily) TableName() string { return "traffic_stat_daily" }
