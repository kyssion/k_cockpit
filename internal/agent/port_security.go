package agent

// OpPortSecurityPrecheck 探测一个网口的端口安全能力与将要下发的规则。**只读**。
//
// 它必须来自节点而不是控制面自己算：
//
//   - **能力是否具备**只有节点知道（OVS 装了没有、OpenFlow13 支持不支持、
//     meter 有没有）。控制面猜错的代价是给用户一个不会生效的"已启用"。
//   - 最终写下去的流表取决于该网口当前所属的网桥、已有的流表与端口号，
//     控制面按模板拼一段出来看起来对、实际可能相差很远。
const OpPortSecurityPrecheck OpKind = "port_security.precheck"

// OpPortSecurityApply 下发端口安全策略。
//
// 下发的是**期望状态**（三项保护各开不开、限速多少），而不是"增删某条规则"。
// 理由是节点上的实际状态可能已经被别人改过（手工加过流表、节点重启过），
// 增量指令在那种情况下会收敛到错误结果，而期望状态无论当前是什么样都能
// 收敛到正确结果。
const OpPortSecurityApply OpKind = "port_security.apply"

// PortSecurityPrecheckKey 是预检结果中承载详情的键。
const PortSecurityPrecheckKey = "port_security_precheck"

// PortSecurityDataKey 是下发结果中承载补充信息的键。
const PortSecurityDataKey = "port_security"

// PortSecurityPrecheck 是节点返回的预检详情。
type PortSecurityPrecheck struct {
	// Capabilities 是三项能力各自的可用状态。
	//
	// 形状与 network 包的能力清单一致（Key / Label / Required / Missing /
	// Reason / Fix），这样界面上可以直接复用同一套呈现。
	Capabilities []PortSecurityCapability
	// Rules 是将要写入的流表，人可读。
	Rules []string
	// Warnings 是节点侧发现的冲突（如该网口已有手工流表会被覆盖）。
	Warnings []string
}

// PortSecurityCapability 是一项能力的可用状态。
type PortSecurityCapability struct {
	Key      string
	Label    string
	Required bool
	Missing  bool
	Reason   string
	Fix      string
}

// PortSecurityInfo 是下发结果。
type PortSecurityInfo struct {
	// Applied 为 true 表示三项保护已按期望状态写入。
	Applied bool
	Message string
}
