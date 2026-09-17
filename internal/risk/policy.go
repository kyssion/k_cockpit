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
