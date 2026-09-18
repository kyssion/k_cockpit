package agent

// OpHostPCIDevices 探测宿主机上的 PCIe 设备与 IOMMU 分组。**只读**。
//
// **分组必须由节点给出**，这对直通是决定性的：IOMMU 分组里的设备只能**一起**
// 绑定到 vfio-pci、一起挂给虚拟机。用户选了显卡之后发现鼠标键盘也一起被拿走
// 了——那正是"同组"的表现，而分组关系只有节点能探测到（它取决于主板拓扑与
// BIOS 设置，控制面无从推断）。
//
// 因此探测结果里必须带 `group_members`：界面上要让用户在看到"我要直通这块卡"
// 的同时，也看到"同组的另外三个设备会一起被拿走"。
const OpHostPCIDevices OpKind = "host.pci.devices"

// OpHostIOMMU 探测 IOMMU 与 VFIO 的可用性。**只读**。
//
// 开启 IOMMU 需要**改内核启动参数并重启**，面板做不了（它自己就跑在这台
// 机器上）。因此这个操作只回答两个问题：现在能不能直通、不能的话要做什么。
// 给指引而不是假装能一键开启——后者会让用户点一下、机器重启、而什么都没变。
const OpHostIOMMU OpKind = "host.iommu"

// OpHostPCIBind 把设备绑定到 vfio-pci（或解绑回宿主驱动）。
//
// 它会**改变宿主机上设备的归属**：绑走之后宿主就看不到那块卡了。如果那块卡
// 正在被宿主使用（例如它是唯一的网卡），绑定会让宿主机**当场失去网络**——
// 而面板正是通过网络管理的。
const OpHostPCIBind OpKind = "host.pci.bind"

// PCIDevicesKey 是设备清单的键。
const PCIDevicesKey = "pci_devices"

// IOMMUStatusKey 是 IOMMU 状态的键。
const IOMMUStatusKey = "iommu_status"

// PCIDevice 是一块 PCIe 设备。
type PCIDevice struct {
	// Address 是 PCI 地址（0000:01:00.0）。它是**在宿主机上定位设备的
	// 唯一标识**——同一台机器上可以有两块完全相同的卡。
	Address string
	// VendorDevice 形如 10de:1eb8，用于展示与查驱动兼容性。
	VendorDevice string
	// Class 是设备类别（VGA / Network / Audio ...）。
	Class string
	// Description 是人类的可读名（如 "NVIDIA Corporation GP104 [GeForce GTX 1080]"）。
	Description string
	// Driver 是当前绑定的驱动。取值 `vfio-pci` 表示**已绑定、可直通**。
	Driver string

	// IOMUGroup 是 IOMMU 分组号。
	//
	// **同一组的设备只能一起直通。** 这是节点探测才有的事实，而它决定了
	// 用户能不能只拿走自己想要的那一块。
	IOMUGroup int
	// GroupMembers 是同组**其它**设备的地址。
	GroupMembers []string

	// CanPassthrough 为 false 时 Reason 说明原因。
	CanPassthrough bool
	Reason         string

	// BoundToVM 是被哪台虚拟机用着（控制面填），空表示没被用。
	//
	// 节点不知道这件事——它只知道设备绑到了哪个驱动。因此这个字段由控制面
	// 在返回前填上，而它很重要：解绑一块正被虚拟机使用的卡会**拔掉那台
	// 运行中机器的"显卡"**。
	BoundToVM string
}

// IOMMUStatus 是直通能力的就绪状态。
type IOMMUStatus struct {
	// Enabled 为 true 表示内核已启用 IOMMU。
	Enabled bool
	// VFIOAvailable 为 true 表示 vfio-pci 模块可用。
	VFIOAvailable bool
	// Reason 说明为什么不可用。
	Reason string
	// Fix 给出**要做什么**（改哪个内核参数、装哪个包、是否需要重启）。
	//
	// 必须是可操作的指令：这一项在面板上做不了，而"不支持"三个字让用户
	// 只能放弃。告诉他加哪个参数、重启之后回来，他几分钟就能解决。
	Fix string
	// NeedReboot 为 true 表示改完参数需要重启宿主机。
	//
	// 必须显式说出来——重启宿主机意味着**上面所有虚拟机会停机**，而用户
	// 在看到这条之前不会意识到那一点。
	NeedReboot bool
}

// PCIBindInfo 是绑定/解绑的结果。
type PCIBindInfo struct {
	Message string
	// Warnings 是节点侧发现的、值得用户先看一眼的问题。
	Warnings []string
}
