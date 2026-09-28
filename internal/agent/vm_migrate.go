package agent

// OpVMMigrate 把一台虚拟机从当前节点迁移到另一台（F-2-09）。
//
// 与其它磁盘操作的区别在于它**横跨两台宿主机**：源侧要读出磁盘，目标侧要
// 写入磁盘，中间还要传输。因此它有两个独立的失败面，而排障时先要分清是哪
// 一侧出的问题——阶段上报里用 `source.` / `target.` 前缀把两侧分开。
//
// 一个贯穿本操作的取向：**源侧的数据在目标侧确认之前不删**。迁移失败时
// 源侧保留完整的数据，用户至少还有一台能用的机器；反过来（先删源再传）
// 失败时会同时失去两侧，而那是最不可接受的结果。
const OpVMMigrate OpKind = "vm.migrate"

// MigrateResultKey 是迁移结果中承载补充信息的键。
const MigrateResultKey = "migration"

// MigrateResult 是节点在迁移完成后回传的信息。
type MigrateResult struct {
	// Moved 说明跟着搬了些什么（网卡、静态地址等），供界面展示。
	//
	// 只给一个「成功」会让用户不确定「我原来接的网络、配的转发还在不在」，
	// 而那是他迁移前最关心的事之一。
	Moved []string
	// DurationSeconds 是实际传输耗时。
	//
	// 由节点上报而不是控制面按任务起止时间算：任务时间包含排队与前后处理，
	// 而用户关心的是「数据搬了多久」——那个数字会直接影响他对后续迁移量级的
	// 判断。
	DurationSeconds int
	// DowntimeMs 是热迁移的停顿窗口（毫秒）：切换到目标侧那一刻机器暂停的
	// 时长。停机迁移没有这个概念（全程都是停的），因此只在 live 模式有值。
	DowntimeMs int `json:"downtime_ms,omitempty"`
}

// OpVMMigrateTakeover 迁移完成后的目标侧接管（F-6-04）。
//
// 数据搬到目标节点只是迁移的一半：虚拟机要在那边"活起来"，还差目标侧
// 的补齐——固件变量（NVRAM）、网络绑定（网桥/端口）、以及目标节点本地
// 需要登记的资源。这些由**目标 agent**执行，而不是源侧代劳：目标侧的
// 状态只有它自己知道。
//
// 时序约束：接管在源侧迁移成功**之后**、控制面改记录**之前**——接管失败
// 时控制面仍指向源节点，源侧数据未清理，重试是安全的。
const OpVMMigrateTakeover OpKind = "vm.migrate.takeover"

// MigrateTakeoverDataKey 是接管结果的键。
const MigrateTakeoverDataKey = "migrate_takeover"

// MigrateTakeoverInfo 是目标侧接管的结果。
type MigrateTakeoverInfo struct {
	// Applied 说明目标侧补齐了哪些东西（界面展示用，与 MigrateResult.Moved
	// 同一口径）。
	Applied []string
	// Message 是节点给的补充说明，可空。
	Message string `json:"message,omitempty"`
}
