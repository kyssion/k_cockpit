// guest_agent.go：域定义里的 QEMU guest agent 通道。
//
// 这个文件存在的理由是一次真机缺陷（第 3 轮 DEF-R3-16）：全仓 `grep "<channel"`
// 为空——域 XML 里**从来没有**这个词道。后果不是"少个便利功能"，而是来宾侧的
// 一整类能力全部不可达：来宾内改密、读取 IP、就绪探测、自动弹出介质，全都依赖
// 宿主机通过 virtio-serial 通道跟 qemu-guest-agent 说话。症状是
// `virsh qemu-agent-command` 直接报 `argument unsupported: QEMU guest agent is
// not configured`，而界面上只会显示"操作失败"。
package nodeagent

import "strings"

// guestAgentChannelName 是 QEMU guest agent 的通道名。
//
// 这是**协议约定值**，不是我们起的名字：qemu-guest-agent 在来宾里就等在这个
// 名字上（`org.qemu.guest_agent.0`），换一个名字两端就对不上。
const guestAgentChannelName = "org.qemu.guest_agent.0"

// guestAgentChannelXML 渲染 channel 元素（indent 是设备级缩进）。
//
// **不写 <source path=...>**，只写 `<source mode='bind'/>`：socket 路径交给
// libvirt 在每域的状态目录里分配（Debian/Ubuntu 是 /var/lib/libvirt/qemu/，
// RHEL 与部分容器环境是 /run/libvirt/qemu/），属主还要跟 qemu 的运行用户与
// apparmor/selinux 策略对上。硬编码其中一条等于把这个功能绑死在一个发行版上，
// 而且失败表现是"define 成功、通道却不可用"——很难查。
// 省略 path 是 libvirt 支持的写法（virt-install 生成的就是这一种）。
func guestAgentChannelXML(indent string) string {
	return indent + "<channel type='unix'>\n" +
		indent + "  <source mode='bind'/>\n" +
		indent + "  <target type='virtio' name='" + guestAgentChannelName + "'/>\n" +
		indent + "</channel>\n"
}

// hasGuestAgentChannel 判断域定义里是否已有 guest agent 通道。
func hasGuestAgentChannel(xmlText string) bool {
	return strings.Contains(xmlText, guestAgentChannelName)
}

// hasVirtioSerialController 判断是否已声明 virtio-serial 控制器。
func hasVirtioSerialController(xmlText string) bool {
	return strings.Contains(xmlText, "type='virtio-serial'") ||
		strings.Contains(xmlText, `type="virtio-serial"`)
}

// ensureGuestAgentChannel 保证域定义里既有 virtio-serial 控制器、也有 agent 通道。
//
// 返回 (新XML, 是否改动)。已经是目标状态时原样返回——补通道不该让每次
// define 都产生一次无谓的写入（幂等）。
//
// **三条产生域定义的路径都要过这里**，否则缺陷会以另一种形式回来：
//   - 创建（BuildDomainXML 直接带，见 xmlbuilder.go）；
//   - 纳管（adopt 读到的既有域通常没有通道，补进去才usable）；
//   - 改 XML（用户在面板里编辑定义时很可能不带 channel——它是"看不见的"）。
func ensureGuestAgentChannel(xmlText string) (string, bool) {
	if hasGuestAgentChannel(xmlText) {
		return xmlText, false
	}
	i := strings.Index(xmlText, "</devices>")
	if i < 0 {
		// 结构不认识（不是标准域定义）：原样返回，让 libvirt 去拒绝它。
		return xmlText, false
	}
	insert := ""
	if !hasVirtioSerialController(xmlText) {
		insert += "    <controller type='virtio-serial' index='0'/>\n"
	}
	insert += guestAgentChannelXML("    ")
	return xmlText[:i] + insert + xmlText[i:], true
}
