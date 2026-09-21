package agent

// ACL 的操作（F-4-05）。与安全组同一套形状：**先预览、后应用**。
const (
	// OpVpcACLPreview 把规则集渲染成节点上真正会生效的条目。
	OpVpcACLPreview OpKind = "vpc.acl.preview"
	// OpVpcACLApply 应用规则集。
	OpVpcACLApply OpKind = "vpc.acl.apply"
)

// VpcACLPreviewDataKey 是预览结果的键。
const VpcACLPreviewDataKey = "vpc_acl_preview"

// VpcACLRuleSpec 是下发给节点的一条规则。
//
// 与控制面的 VpcACLRule 形状一致但**不带控制面字段**（ID、备注、创建时间）:
// 节点只需要"匹配什么、怎么处理、第几条"，多传的字段会在它被归并成流表
// 时变成无意义的噪声。
type VpcACLRuleSpec struct {
	Priority  int
	Action    string
	Direction string
	Protocol  string
	SrcCIDR   string
	DstCIDR   string
	PortStart int
	PortEnd   int
}

// VpcACLPreview 是预览结果。
type VpcACLPreview struct {
	// Rendered 是节点上真正会生成的条目（如流表 / 规则文本）。
	//
	// 展示它的理由：规则集在"我填了什么"与"实际生效什么"之间隔着一层归并
	// （同优先级排序、地址展开、协议归一化），不展示这一步，用户只能在
	// 网络不通之后才发现问题。
	Rendered []string
	// Warnings 是隐患提示，例如"第 2 条匹配全部且为 deny，后面的规则不会生效"。
	Warnings []string
	// Count 是生效的规则条数。
	Count int
	// Version 是这次预览的指纹。
	//
	// 应用必须带回它：如果预览之后规则又变了，按旧预览去应用等于用一个没人
	// 看过的结论去改实际的网络——安全组那边（F-4-04）用的是同一条约定。
	Version string
	// Unavailable 非空表示节点不支持 ACL。
	Unavailable string
}

// VpcACLDataKey 是应用结果的键。
const VpcACLDataKey = "vpc_acl"

// VpcACLApplyInfo 是应用结果。
type VpcACLApplyInfo struct {
	Applied int
	Message string
}
