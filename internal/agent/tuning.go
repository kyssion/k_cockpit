package agent

// OpHostTuning 读取宿主机的性能调优状态。**只读**。
//
// 它返回的**不只是开关**，还有每一项的**实际效果**。这一点是本操作最重要
// 的设计：
//
//	KSM 打开之后有没有用？——看它合并了多少页、省了多少内存。
//	ZRAM 打开之后有没有用？——看压缩率与换出的量。
//
// 只给一个「已启用」的开关，用户无法判断该不该开、开了有没有效果。而 KSM
// 是一个**持续消耗 CPU** 的机制：它在什么都没合并的时候照样扫描内存。因此
// 「开着但一无所获」是一个真实存在的、且用户看不出来的坏状态。
const OpHostTuning OpKind = "host.tuning"

// OpHostTuningApply 修改一项调优配置。
//
// 每一项的下发方式都不同（写 sysfs、重载模块、zramctl），因此参数里带
// `item` 与该项自己的字段——而不是拆成十几个 Kind：它们读的是同一份状态，
// 而拆开会让"读取一份状态"实现十几遍。
const OpHostTuningApply OpKind = "host.tuning.apply"

// TuningStateKey 是调优状态的键。
const TuningStateKey = "tuning"

// TuningState 是宿主机的调优状态。
type TuningState struct {
	KSM    KSMState
	ZRAM   ZRAMState
	Nested NestedState
}

// KSMState 是内核同页合并的状态。
type KSMState struct {
	Enabled bool
	// PagesShared 是被合并后**实际只存一份**的页数。
	PagesShared int64
	// PagesSharing 是有多少个虚拟页指向了那些共享页。
	//
	// 它与 PagesShared 的差就是**省下的页数**——这个差值才是 KSM 的收益，
	// 而单看任何一个都读不出收益。
	PagesSharing int64
	// PagesUnshared 是扫描过、本该能合并但暂时还不能的页。
	PagesUnshared int64
	// SavedBytes 是估算省下的内存。
	//
	// **由节点算好**，不让控制面按页大小推：页大小在不同架构上不同
	// （x86 通常 4K，arm64 可能是 64K），而控制面猜错的话，界面上那个
	// "省了 8 倍内存"的数字看起来很有说服力却完全是错的。
	SavedBytes int64
	// FullScans 是完整扫描的轮次。
	//
	// 它用来回答一个具体的问题：**"开了但省了 0"是正常还是异常**。
	// 扫描轮次为 0 说明它还没跑完第一轮（正常，刚开）；轮次很多而收益为 0
	// 说明这台机器上本来就没有可合并的页（那这个开关应当关掉，它在白耗 CPU）。
	FullScans int64
	// RunMode 是节点的模式说明（如 "always" / "madvise"）。
	RunMode string
}

// ZRAMState 是 ZRAM 压缩内存的状态。
type ZRAMState struct {
	Enabled bool
	// DisksizeBytes 是分配给 ZRAM 的容量。
	DisksizeBytes int64
	// UsedBytes 是压缩后**实际占用**的物理内存。
	UsedBytes int64
	// OrigDataBytes 是压缩前的原始数据量。
	//
	// 它与 UsedBytes 的比值就是压缩率，而那是 ZRAM 收益的唯一度量。
	OrigDataBytes int64
	// Algorithm 是压缩算法（lz4 / zstd）。
	//
	// **算法是一个需要用户做决定的取舍**：lz4 更快但压缩率低，zstd 压缩率
	// 高但更吃 CPU。只给一个"算法"下拉而不说代价，用户只能凭感觉选。
	Algorithm string
	// MemLimitBytes 是"内存用满多少才开始压缩"。
	MemLimitBytes int64
}

// NestedState 是嵌套虚拟化的状态。
type NestedState struct {
	// Enabled 表示当前生效值。
	Enabled bool
	// Supported 为 false 时 Reason 说明原因（如宿主 CPU 不支持）。
	Supported bool
	Reason    string
	// Persistent 为 false 表示重启后会失效。
	//
	// 必须显式说出来：用户改完看到"已启用"，重启之后又变回去了——
	// 而那时他会以为是面板没保存成功。
	Persistent bool
	// Fix 给出持久化要做什么。
	Fix string
}
