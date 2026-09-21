package agent

// 模板派生链的四个维护动作（F-3-04 的后续迭代）。
//
// 派生链（链式克隆）意味着一个模板的盘可能是"父盘的差异层"。链越长，读一次
// 要串起来的层越多，也越脆弱——中间任何一代丢了，下游全部不可用。这四个动作
// 都是在**改动这条链**，因此它们必须显式、可追踪，而不是让用户在别处顺手删掉
// 中间一代。
const (
	// OpTemplateRebase 把模板的 backing 切换到它的上级：缩短链，代价是要把
	// 差异写回一次。
	OpTemplateRebase OpKind = "template.rebase"
	// OpTemplateFlatten 在线拉平到上级：与 rebase 同方向，但**不中断**正在
	// 使用这块盘的虚拟机。
	//
	// 与 rebase 分开：一个要停机、一个不要，界面上给用户的说明完全不同。
	OpTemplateFlatten OpKind = "template.flatten"
	// OpTemplatePromoteChild 把某个子模板提升一级（挂到当前模板的父上）。
	OpTemplatePromoteChild OpKind = "template.promote_child"
	// OpTemplatePromoteDelete 删除中间一代，并把它的子模板重挂到上级。
	//
	// 这是"删掉中间一代"唯一安全的做法：直接删除会让下游全部失效，而节点
	// 上的表现是"虚拟机还在跑，但读某个块时报错"——那要等到数据被访问才暴露。
	OpTemplatePromoteDelete OpKind = "template.promote_delete"
)

// TemplateMaintainDataKey 是维护动作结果的键。
const TemplateMaintainDataKey = "template_maintain"

// TemplateMaintainInfo 是维护动作的结果。
type TemplateMaintainInfo struct {
	// Affected 是受影响的派生模板数。
	Affected int
	// RewrittenBytes 是本次实际改写的数据量（拉平 / rebase 才有）。
	//
	// 回显它是为了让界面能说清"这次到底搬了多少数据"——用户据此判断要不要
	// 挑个业务低峰来做。
	RewrittenBytes int64
	Message        string
	// Rollback 为 true 表示失败并已回滚到原链。
	Rollback bool
}
