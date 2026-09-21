package agent

// 公网 IP 的三项运维能力（F-4-06 的后续迭代）。
//
// 它们的共同点是"节点上有一份真实状态，而控制面只有记录"：
//   - 前缀：节点能读到本地网卡实际配置的 IPv6 前缀，控制面只能存用户填的；
//   - 规则：节点上真正生效的 NAT / 路由规则可能与记录漂移（手工改过）；
//   - 来宾：虚拟机里有没有真的配上这个地址，只有节点（或来宾）知道。
const (
	// OpPublicIPv6Detect 检测宿主机外网网卡上的 IPv6 前缀。
	//
	// 为什么要检测而不是让用户手填：一个 /64 前缀能生成 2^64 个地址，手填
	// 既容易错、也无法回答"还剩多少可用"。
	OpPublicIPv6Detect OpKind = "public_ip.ipv6_detect"
	// OpPublicIPReload 按当前绑定关系重新应用全部规则。
	//
	// 它对应"记录是对的、节点上漂了"这一场景，因此**不需要参数**：参数是
	// "要改成什么"，而重载做的是"让它与我们记录的一致"。
	OpPublicIPReload OpKind = "public_ip.reload"
	// OpPublicIPGuestStatus 查询虚拟机里实际配置的公网地址。
	OpPublicIPGuestStatus OpKind = "public_ip.guest_status"
)

// IPv6PrefixDataKey 是前缀检测结果的键。
const IPv6PrefixDataKey = "ipv6_prefix"

// IPv6PrefixInfo 是一个检测到的 IPv6 前缀。
type IPv6PrefixInfo struct {
	// Prefix 形如 2001:db8:1::/64。
	Prefix string
	// EgressIf 是它所在的出口网卡。
	EgressIf string
	// Trusted 表示节点认为这个前缀可用（已通过可达性检查）。
	//
	// 它来自节点而不是控制面："能不能用"取决于本地路由与上游通告，控制面
	// 无从判断。
	Trusted bool
	// Assignable 是还能分配的地址数估算；-1 表示未知。
	Assignable int64
}

// PublicIPReloadDataKey 是重载结果的键。
const PublicIPReloadDataKey = "public_ip_reload"

// PublicIPReloadInfo 是重载的结果。
type PublicIPReloadInfo struct {
	// Applied 是重新下发的规则条数。
	Applied int
	// Failed 是失败的条数，Detail 给出失败原因。
	Failed  int
	Detail  string
	Message string
}

// GuestIPDataKey 是来宾地址状态的键。
const GuestIPDataKey = "guest_ip"

// GuestIPInfo 是一台虚拟机里实际配置的公网地址。
type GuestIPInfo struct {
	// Address 是地址（含掩码或前缀长度）。
	Address string
	// Family 取值 ipv4 / ipv6。
	Family string
	// Configured 表示来宾里真的配上了。
	//
	// 绑定成功 ≠ 来宾里配好了：控制面只保证规则被下发，来宾里还要有人（或
	// cloud-init）把地址配上。没有这一项，"绑了却不通"无从判断卡在哪一步。
	Configured bool
	// Reachable 表示节点能连通这个地址。
	Reachable bool
	Detail    string
}
