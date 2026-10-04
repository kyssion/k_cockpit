package agent

// 存量虚拟机纳管（节点接入后的第一步之一）。
//
// 节点上可能已经有 libvirt 域——在面板之外手工创建、或从前一套管理体系
// 遗留。它们不归面板管（没有投影、没有归属、没有配额记账），但占用着
// 真实的 CPU / 内存 / 磁盘。纳管 = 让控制面为既有域补一份记录，**不改动
// 域本身**：虚拟化层里它是什么样，纳管之后还是什么样。
const (
	// OpNodeDomains 列出节点上虚拟化层的全部域。
	//
	// 只读探测：不区分「面板管的」与「面板外的」——那是控制面按自己的
	// 记录过滤的事，节点根本不知道哪些域有投影。返回的 State / VCPU /
	// MemoryMB 来自域的**实际定义与运行态**，纳管记录以此为底，而不是
	// 让用户手填一遍再对不上账。
	OpNodeDomains OpKind = "node.domains"

	// OpVMAdopt 把一个既有域登记到面板名下。
	//
	// 看似纯控制面动作，但仍要通知节点：节点侧要标记该域进入托管集合
	// （后续的状态上报、指标采集、对账都以它为准），并回传域的 UUID——
	// 控制面自己读不到 libvirt 的 UUID，而它是节点内唯一且稳定的标识，
	// 比名字更适合做对账键。
	OpVMAdopt OpKind = "vm.adopt"
)

// NodeDomainsDataKey 是域列表结果的键。
const NodeDomainsDataKey = "node_domains"

// VMAdoptDataKey 是纳管结果的键。
const VMAdoptDataKey = "vm_adopt"

// DomainInfo 是节点上发现的一个域。
type DomainInfo struct {
	Name string `json:"name"`
	// State 是运行态，取值与 OpVMStatus 的口径一致（running / stopped / …）。
	State string `json:"state"`
	VCPU  int    `json:"vcpu"`
	// MemoryMB 是域定义的内存上限，不是当前用量。
	MemoryMB int `json:"memory_mb"`
	// DiskGB 是系统盘与数据盘容量之和（向上取整到 GB）。
	DiskGB int `json:"disk_gb"`
	// Autostart 表示域是否随 libvirt 自启。
	Autostart bool `json:"autostart"`
}

// VMAdoptInfo 是纳管的结果。
type VMAdoptInfo struct {
	// UUID 是域在 libvirt 里的 UUID，纳管后由控制面记录为对账键。
	UUID string `json:"uuid"`
}
