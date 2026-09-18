package agent

// OpQuotaEnforce 对某用户的网络施加或撤销配额处置（F-4-10）。
//
// 它下发的是**期望状态**（该限速 / 该断网 / 该恢复），而不是"加一条规则 /
// 删一条规则"。理由与端口安全一致：节点上的实际状态可能已经被别人改过
// （手工加过流控、节点重启过），增量指令在那种情况下会收敛到错误结果。
//
// 与其它操作的另一个不同：它**按虚拟机集合生效**，而集合会变（用户新建
// 一台机器）。因此控制面每次判定都要重新下发，而不是发一次就完事——
// 新机器不会自动被上一次的规则覆盖。
const OpQuotaEnforce OpKind = "quota.enforce"

// QuotaDataKey 是结果中承载补充信息的键。
const QuotaDataKey = "quota"

// QuotaInfo 是处置结果。
type QuotaInfo struct {
	// Applied 为 true 表示处置已生效（含"已撤销"）。
	Applied bool
	// Affected 是受影响的虚拟机数量。
	//
	// 它是用户最关心的一个数：限速作用到了几台机器上。为 0 时说明这个
	// 用户当前没有运行中的机器——那是"处置已记录但暂无实际影响"，
	// 与"处置失败"是两回事。
	Affected int
	Message  string
}
