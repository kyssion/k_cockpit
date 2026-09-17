package agent

// 公网 IP 相关操作（F-4-06）。
const (
	// OpPublicIPChange 把公网地址的绑定状态同步到节点。
	//
	// 一个操作承载绑定与解绑（由 Params["action"] 区分），与
	// vm.interface.change 同一形状：对节点而言「让这个地址指向那里」与
	// 「让这个地址不指向任何地方」是同一件事的两个取值。
	OpPublicIPChange OpKind = "public.ip.change"

	// OpPublicIPPreview 计算将要下发的规则，但**不应用**。
	//
	// f-4-06 明确要求「规则预览」。它必须来自节点而不是控制面拼出来：
	// 规则的最终形态取决于宿主机上已有的 iptables/nftables 链、路由表与
	// 网卡配置——控制面按模板拼一段出来，看起来对、实际可能与真实规则
	// 相差很远，而用户正是拿这份预览去做「改还是不改」的判断。
	OpPublicIPPreview OpKind = "public.ip.preview"
)

// PublicIPDataKey 是绑定结果中承载补充信息的键。
const PublicIPDataKey = "public_ip"

// PublicIPPreviewKey 是预览结果中承载规则文本的键。
const PublicIPPreviewKey = "public_ip_preview"

// PublicIPInfo 是节点在绑定/解绑完成后回传的信息。
type PublicIPInfo struct {
	// Message 面向用户的补充说明。
	Message string
}

// PublicIPPreview 是节点算出的规则预览。
//
// 分「新增」与「移除」两组而不是一段合并的文本：用户要判断的是
// 「这次改动会不会影响我现有的规则」，把两类混在一起会让这个判断变难。
type PublicIPPreview struct {
	// Added 是本次改动**将要新增**的规则。
	Added []string
	// Removed 是本次改动**将要移除**的规则。
	Removed []string
	// Warnings 是节点发现的、值得用户先看一眼的问题。
	//
	// 例如「该地址已在宿主机上被一条手工规则占用」——这类情况不会让
	// 绑定失败，但会让结果与预期不符，而事后很难追查。
	Warnings []string
}
