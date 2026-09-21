// Package risk 实现高风险操作的二次验证（F-10-01 / F-10-02）。
//
// 判定口径是「会造成**不可逆**结果 或 **影响可达性**」：这两类后果无法靠
// 事后补救挽回（数据没了、机器连不上了），其余问题都可以用确认弹窗与回滚
// 机制解决。
//
// 与之相对，查询类与普通创建类**不列入**：创建虚拟机是可逆的（删除即可），
// 把它列入只会让用户对验证麻木——一旦养成无脑点确认的习惯，真正危险的操作
// 反而会被忽略。
package risk

// Action 标识一个受保护的操作。
//
// 取值与任务类型、审计 action 保持同一命名风格（资源域.动作），便于把
// 「哪次操作被验证过」与「哪条审计记录」对应起来。
type Action string

// 受保护的操作。
const (
	// 删除类：数据没了就是没了。
	ActionVMDelete Action = "vm.delete"
	// 移除节点：其上的虚拟机将失去管控，属于**影响可达性**。
	ActionNodeRemove Action = "node.remove"
	// 撤销会话：被撤销方立即失去访问，同样属于影响可达性。
	ActionSessionRevoke Action = "session.revoke"
	// 创建存储池：会**格式化**设备，原有数据无法恢复。
	ActionStoragePoolCreate Action = "storage.pool.create"
	// 删除存储池：销毁其中的磁盘。
	ActionStoragePoolDelete Action = "storage.pool.delete"
	// 控制台对外暴露：把宿主机端口开放到网络，等于给这台虚拟机开了一扇
	// 绕过面板的后门（f-2-08 R-004 / Q-007）。
	ActionConsoleExpose Action = "vm.console.expose"

	// 直接编辑虚拟机的 libvirt 定义。
	//
	// **它绕过我们建立的其它全部校验**：同节点、配额、地址唯一性、端口安全的
	// 前置条件——在 XML 里都可以被绕开。因此它比暴露控制台更需要验证，而不是
	// 同样需要：暴露的后果是"多开了一个入口"，而这里是可以把一台机器的电源、
	// 磁盘、网络改成任何样子。
	ActionVMXMLEdit Action = "vm.xml.edit"

	// 解除虚拟机的业务软锁（F-2-12）。
	//
	// **加锁不需要验证，解锁需要**——这个不对称是刻意的：锁的作用就是让
	// 「删除」这件事必须先经过一道明确的动作。如果解锁和加锁一样是一次
	// 普通点击，它就只是减速带，防不住「看错行、顺手删掉」这类事故。
	ActionVMLockRelease Action = "vm.lock.release"

	// 恢复快照（F-2-07）。
	//
	// 它会**丢弃快照之后的所有磁盘改动**，且不可撤销——属于「不可逆结果」
	// 这一类，与删除同级。用户点下恢复时往往想的是「回到那个时间点」，
	// 而实际发生的是一次不可逆的回滚。
	ActionVMSnapshotRestore Action = "vm.snapshot.restore"

	// 删除全部快照（F-2-07）。
	//
	// 单条删除只丢一个还原点，而"删除全部"丢的是**整条时间线**——它通常
	// 发生在"快照太多了想清空"这种想法下，而那一刻用户多半没有逐条确认过
	// 里面有没有还要用的。因此它与恢复快照同级，需要一次明确的验证。
	ActionVMSnapshotDeleteAll Action = "vm.snapshot.delete_all"

	// 下载包含明文密码的控制台连接文件（F-2-09）。
	//
	// 控制台密码平时是「只写不读」的（R-005）。把它写进 .vv 文件等于让
	// 明文离开服务端，因此这一条路必须明确经过一次验证，并且是可选项——
	// 默认生成的连接文件不含密码，用户手输即可。
	ActionConsoleConnectionFile Action = "vm.console.connection_file"

	// 分区操作（F-5-01 的后续迭代）。
	//
	// 改分区表会影响**整块磁盘**，而且失败往往要到下次读写才发现——那正是
	// 需要一次明确确认的场景。
	ActionStoragePartitionCreate Action = "storage.partition.create"
	ActionStoragePartitionDelete Action = "storage.partition.delete"
	// ActionStoragePartitionDeleteAll 与删单个分区分开登记：理由完全不同——
	// 一个是"少一个分区"，一个是"整张分区表没了"。
	ActionStoragePartitionDeleteAll Action = "storage.partition.delete_all"
	// ActionStoragePoolUnmount 卸载存储池。
	ActionStoragePoolUnmount Action = "storage.pool.unmount"

	// 重装系统（F-2-11）。
	//
	// 它会**替换整块系统盘**——原系统上的所有配置、安装的软件、没放在数据盘
	// 上的数据都随之消失。比删除轻一档（机器还在、数据盘还在），但同样不可逆。
	ActionVMReinstall Action = "vm.reinstall"
)

// Entry 是清单中的一条，用于对外下发（API-034）。
type Entry struct {
	Action Action `json:"action"`
	Label  string `json:"label"`
	// Reason 说明「为什么这个操作需要验证」，供前端在提示中解释缘由。
	// 只写「该操作需要验证」而不说原因，用户只会感到被阻挠。
	Reason string `json:"reason"`
}

// policy 是高风险操作清单。
//
// **判定集中在这里**（R-001）：各能力不得自行决定某个操作是否需要验证。
// 分散判定会让清单随时间失控——每处都「顺手加一个」，最终每个操作都要
// 验证，防线形同虚设。
var policy = []Entry{
	{
		Action: ActionVMDelete,
		Label:  "删除虚拟机",
		Reason: "连同磁盘删除时数据无法恢复",
	},
	{
		Action: ActionNodeRemove,
		Label:  "移除节点",
		Reason: "该节点上的虚拟机将失去管控",
	},
	{
		Action: ActionSessionRevoke,
		Label:  "撤销会话",
		Reason: "被撤销的登录会立即失效",
	},
	{
		Action: ActionStoragePoolCreate,
		Label:  "创建存储池",
		Reason: "会格式化所选设备，其上的原有数据无法恢复",
	},
	{
		Action: ActionStoragePoolDelete,
		Label:  "删除存储池",
		Reason: "池内的磁盘会被一并销毁",
	},
	{
		Action: ActionConsoleExpose,
		Label:  "对外暴露控制台",
		Reason: "将向网络开放宿主机端口，可绕过面板直接接入该虚拟机",
	},
	{
		Action: ActionVMXMLEdit,
		Label:  "直接编辑虚拟机定义",
		Reason: "绕过配额、地址唯一性与前置条件校验，可把电源、磁盘、网络改成任意状态",
	},
	{
		Action: ActionVMLockRelease,
		Label:  "解锁虚拟机",
		Reason: "解锁后该虚拟机即可被删除，锁定提供的保护随之消失",
	},
	{
		Action: ActionVMSnapshotRestore,
		Label:  "恢复快照",
		Reason: "快照之后产生的磁盘改动会被丢弃，且无法撤销",
	},
	{
		Action: ActionVMSnapshotDeleteAll,
		Label:  "删除全部快照",
		Reason: "这台虚拟机的所有还原点会被一次性删除，无法撤销",
	},
	{
		Action: ActionConsoleConnectionFile,
		Label:  "下载含密码的控制台连接文件",
		Reason: "文件里会带上控制台密码的明文",
	},
	{
		Action: ActionStoragePartitionCreate,
		Label:  "创建分区",
		Reason: "磁盘的分区表会被改写，失败往往要到下次读写才暴露",
	},
	{
		Action: ActionStoragePartitionDelete,
		Label:  "删除分区",
		Reason: "该分区上的数据会被清除，且无法撤销",
	},
	{
		Action: ActionStoragePartitionDeleteAll,
		Label:  "删除全部分区",
		Reason: "整块磁盘的分区表会被清空，其上所有数据都会丢失",
	},
	{
		Action: ActionStoragePoolUnmount,
		Label:  "卸载存储池",
		Reason: "该池上的虚拟机将无法读写磁盘，直到重新挂载（数据保留）",
	},
	{
		Action: ActionVMReinstall,
		Label:  "重装系统",
		Reason: "整块系统盘会被替换，原系统上的软件与配置全部消失（数据盘保留）",
	},
}

// protectedSet 是 policy 的索引，避免每次判定都遍历切片。
var protectedSet = func() map[Action]struct{} {
	set := make(map[Action]struct{}, len(policy))
	for _, e := range policy {
		set[e.Action] = struct{}{}
	}
	return set
}()

// Protected 报告某操作是否需要二次验证。
//
// 判定是常量级查表，不引入额外查询（F-10-01 §6 性能要求）。
func Protected(a Action) bool {
	_, ok := protectedSet[a]
	return ok
}

// Policy 返回完整清单。
//
// 返回副本而非切片本身：调用方拿到后可能排序或裁剪，共享底层数组会让
// 这些操作影响后续所有调用者。
func Policy() []Entry {
	out := make([]Entry, len(policy))
	copy(out, policy)
	return out
}
