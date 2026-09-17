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
}
