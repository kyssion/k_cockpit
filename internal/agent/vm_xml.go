package agent

// OpVMXML 读取虚拟机的 libvirt 定义。**只读**。
//
// 它是排查时**唯一的真相**：面板上的配置是控制面的意图，而这份 XML 是节点
// 上实际在跑的东西。两者不一致时（例如某人手工 virsh edit 过、或某次下发
// 部分失败），只有这一份能说明问题。因此它总是**现读**，不缓存——缓存会让
// 它变成"上次读到的真相"，而那正好掩盖了要排查的那类变化。
const OpVMXML OpKind = "vm.xml"

// VMXMLKey 是 XML 内容的键。
const VMXMLKey = "vm_xml"

// VMXMLInfo 是读取结果。
type VMXMLInfo struct {
	// XML 是 libvirt 的域定义。
	//
	// **节点侧不做脱敏**——它只是把定义原样返回。脱敏在控制面做，
	// 因为"哪些字段算敏感"是控制面的口径（见 vm.RedactXML），
	// 而两处各做一遍迟早会出现"节点脱了一些、控制面又脱了一些，
	// 合起来漏了第三种"。
	XML string
	// Live 为 true 表示这是**运行中**的定义（含热插拔后的实际状态）。
	//
	// 它与持久定义（`--inactive`）可能不同：热插拔一块磁盘之后，运行中的
	// 定义有它、而持久定义没有——重启之后那块盘就消失了。因此两份都要能看，
	// 而"看的是哪一份"必须标出来。
	Live bool
}

// OpVMXMLApply 校验或应用一份新的域定义。
//
// 两个动作共用一个 Kind，因为它们读的是同一份东西（用户提交的 XML），而
// 校验是应用的前置步骤——拆成两个 Kind 会让"只校验不应用"变成一个需要记得
// 单独实现的路径。
//
// 节点侧必须遵守两条：
//
//  1. **校验用 libvirt 自己的校验器**（`virsh define --validate` 或
//     `virt-xml-validate`），不要自己写规则。控制面做不到这件事的原因很直接：
//     能不能接受这份定义取决于**这台机器上的 libvirt 版本、可用的设备与
//     后端**——同一份 XML 在另一台机器上可能是合法的。
//
//  2. **应用要么全成、要么完全不动**。libvirt 的 define 本身是原子的（校验
//     失败或写入失败都不会改动现有定义），节点侧不要再自己写一套"备份-还原"
//     ——那比 libvirt 自己的保证弱，而且多了一条会出错的路径。
const OpVMXMLApply OpKind = "vm.xml.apply"

// VMXMLValidateKey 是校验结果的键。
const VMXMLValidateKey = "vm_xml_validate"

// VMXMLValidateInfo 是校验结果。
type VMXMLValidateInfo struct {
	// Valid 为 false 时 Errors 说明原因。
	Valid bool
	// Errors 是校验器给出的问题（原样返回，不改写）。
	//
	// **不改写**：libvirt 的报错里有行号与元素名，而改写之后那些信息往往
	// 就丢了——而用户正是靠它们去定位自己改坏了哪一行。
	Errors []string
	// Warnings 是校验通过但值得看一眼的东西（如"下次重启才会生效"）。
	Warnings []string
}
