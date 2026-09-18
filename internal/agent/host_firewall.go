package agent

// OpHostFirewallApply 下发宿主机防火墙的期望状态（F-4-11 第一层）。
//
// 它作用在**宿主机自己**身上——包括 SSH 与面板端口。这意味着一次错误的
// 下发可以直接切断管理通道，因此节点侧的实现必须遵守两条：
//
//  1. **先写入、后收紧**：规则与链先建好，最后才改 INPUT 的默认策略。
//     反过来的话，中间那一瞬间没有任何规则放行，而管理连接可能恰好断在
//     那一瞬间——连接断掉之后再也建不起来。
//  2. **必须支持 rollback**：撤掉本系统写入的全部规则。这是管理员在被
//     自己锁在外面时唯一的自救入口，而它要在"已经出事了"的那一刻还能用。
const OpHostFirewallApply OpKind = "host_firewall.apply"

// OpHostConnections 读取宿主机当前的入站连接。**只读**。
//
// 带 `close` 参数时表示关闭指定连接——那是写操作，但复用一个 Kind 是刻意
// 的：两者读的是同一份数据（`ss` / `netstat` 的输出），拆成两个 Kind 会让
// 连接解析实现两遍。
const OpHostConnections OpKind = "host.connections"

// HostFirewallDataKey 是下发结果中承载补充信息的键。
const HostFirewallDataKey = "host_firewall"

// HostConnectionsKey 是连接清单的键。
const HostConnectionsKey = "connections"

// HostFirewallInfo 是下发结果。
type HostFirewallInfo struct {
	// AppliedRules 是实际写入的规则条数。
	AppliedRules int
	Message      string
	Warnings     []string
}

// HostConnection 是一条入站连接。
//
// 字段刻意**只保留排查需要的**：来源地址、本地端口、状态、所属进程。
// 不返回更多是因为这份数据来自宿主机、每次探测都会变，而"多存一点"只会
// 让界面上出现一堆没人看的列。
type HostConnection struct {
	// RemoteAddr 是远端地址（IP:端口）。它是关闭连接时的**唯一标识**。
	RemoteAddr string
	LocalPort  int
	Protocol   string
	State      string
	// Process 是占用该连接的进程名——判断"这是不是我自己那条 SSH"要靠它。
	Process string
	// Own 为 true 表示这条连接来自当前请求方。
	//
	// 节点无从知道请求方是谁，因此这个标记由**控制面**在返回前填上。
	// 关掉自己那条连接会让人以为面板挂了，而界面上必须能提前看出来。
	Own bool
}
