package agent

// OpSecurityGroupApply 把一台虚拟机的**生效规则**下发到节点（F-4-04）。
//
// 下发的是已经汇总去重的结果，而不是「挂了哪几个组」：汇总要读数据库，
// 那是控制面的事。让节点也做一遍，两处实现迟早分叉——而分叉之后
// 「界面上看到的」与「实际生效的」就不再是同一套东西了。
//
// 规则是**整台虚拟机**的，不是单个网口的：用户问的是「这台机器放行了
// 什么」，按网口分开发下会让同一个问题有两个答案。
const OpSecurityGroupApply OpKind = "security_group.apply"

// SecurityGroupDataKey 是结果中承载补充信息的键。
const SecurityGroupDataKey = "security_group"

// SecurityGroupInfo 是节点回传的信息。
type SecurityGroupInfo struct {
	// Applied 是实际写入的规则条数。
	//
	// 与请求里的条数分开：节点可能因为规则重复或与既有链合并而少写几条，
	// 界面显示「请求 12 条、实际 11 条」能让这种差异立刻可见——而它如果
	// 只体现在宿主机上，用户会以为控制面说谎。
	Applied int
	Message string
}
