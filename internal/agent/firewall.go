package agent

// 防火墙相关操作（F-4-11）。
//
// 两者都是**同步调用**，不经任务队列——见 firewall.Service.Apply 的说明。
// 其中回滚尤其不能排队：被自己配错的防火墙关在门外的管理员，此刻唯一的
// 诉求是「先让我进去」，而排在几十个虚拟机创建之后的回滚帮不了他。
const (
	// OpFirewallApply 把节点级策略与规则写入宿主机的防火墙链。
	OpFirewallApply OpKind = "firewall.apply"

	// OpFirewallRollback 紧急关闭防火墙并撤销本次下发。
	//
	// 它**不应该失败**：节点侧在收到这个操作时应当尽最大努力恢复到
	// 「不拦截」的状态，即使规则链已经被改坏。一个恢复入口如果自己会
	// 失败，它就不是恢复入口。
	OpFirewallRollback OpKind = "firewall.rollback"
)

// FirewallDataKey 是结果中承载补充信息的键。
const FirewallDataKey = "firewall"

// FirewallInfo 是节点回传的信息。
type FirewallInfo struct {
	// Applied 是实际写入的规则条数。
	//
	// 与请求里的条数分开：节点可能因为规则重复或与既有链合并而少写几条，
	// 界面显示「请求 12 条、实际 11 条」能让这种差异立刻可见。
	Applied int
	Message string
}
