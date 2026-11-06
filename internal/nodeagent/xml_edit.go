// xml_edit.go：域 XML 的定点编辑（磁盘 / 网卡）。
//
// 这里只做**字符串级**的定点替换，不引入完整 XML 解析器：libvirt dumpxml
// 的排版稳定，且我们只改单个属性或单个设备块——整份反序列化再序列化会把
// 用户没改的部分也重排，diff 与审计都会失去意义。
//
// 所有函数都是纯函数（输入 XML、输出 XML），便于用真机 dumpxml 片段做单测。
package nodeagent

import (
	"fmt"
	"strconv"
	"strings"

	"k_cockpit/internal/nodeagent/sys"
)

// xmlBlocks 返回 xml 中每个 startTag…endTag 块的原文。
//
// 只处理 libvirt 这种规则的、不嵌套同名标签的输出；域名与设备块都满足。
func xmlBlocks(xml, startTag, endTag string) []string {
	var out []string
	rest := xml
	for {
		i := strings.Index(rest, startTag)
		if i < 0 {
			return out
		}
		j := strings.Index(rest[i:], endTag)
		if j < 0 {
			return out
		}
		out = append(out, rest[i:i+j+len(endTag)])
		rest = rest[i+j+len(endTag):]
	}
}

// attrValue 从一段**单个标签**（如 <target dev='vda' bus='virtio'/>）里取属性。
//
// 单引号与双引号都认：libvirt 输出用单引号，用户手写的片段可能是双引号。
func attrValue(tag, attr string) string {
	for _, q := range []string{"'", `"`} {
		key := attr + "=" + q
		i := strings.Index(tag, key)
		if i < 0 {
			continue
		}
		rest := tag[i+len(key):]
		if k := strings.Index(rest, q); k >= 0 {
			return rest[:k]
		}
	}
	return ""
}

// attrOfTag 在设备块里找到指定子标签，再取它的属性。
func attrOfTag(block, tagName, attr string) string {
	i := strings.Index(block, "<"+tagName)
	if i < 0 {
		return ""
	}
	end := strings.IndexByte(block[i:], '>')
	if end < 0 {
		return ""
	}
	return attrValue(block[i:i+end+1], attr)
}

// setTagAttr 设置一个标签上的属性值；属性不存在时插到标签结束前。
//
// 保留原有引号风格：改一个值不应把整行引号换掉（diff 里会多出一行噪音）。
func setTagAttr(tag, attr, value string) string {
	for _, q := range []string{"'", `"`} {
		key := attr + "=" + q
		i := strings.Index(tag, key)
		if i < 0 {
			continue
		}
		rest := tag[i+len(key):]
		if k := strings.Index(rest, q); k >= 0 {
			return tag[:i] + key + value + rest[k:]
		}
	}
	end := strings.Index(tag, "/>")
	if end < 0 {
		end = strings.Index(tag, ">")
	}
	if end < 0 {
		return tag
	}
	q := "'"
	if strings.Contains(tag, `"`) && !strings.Contains(tag, "'") {
		q = `"`
	}
	return tag[:end] + " " + attr + "=" + q + value + q + tag[end:]
}

// graphicsElement 是 <graphics> 元素在文档中的位置。
//
// **必须区分「开标签」与「整段元素」**：libvirt 的 dumpxml 输出里 graphics
// 通常**带子元素**：

//	<graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
//	  <listen type='address' address='127.0.0.1'/>
//	</graphics>
//
// 旧实现按第一个 "/>" 收尾，于是把内层 <listen/> 也算了进来。改属性时
// 落到内层标签上，产出 `</input> </graphics>` 这类标签错配，define 直接被
// libvirt 拒绝（第 3 轮 DEF-R3-18：改控制台口令 100% 失败）。
type graphicsElement struct {
	// start..openEnd 是开标签（含 <graphics 与收尾的 '>' 或 '/>'）。
	start, openEnd int
	// end 是整段元素的结束下标（自闭合 = openEnd；带子元素 = 闭标签之后）。
	end int
	// openTag 是开标签原文，属性修改只允许作用在它上面。
	openTag string
}

// findGraphicsElement 定位指定协议的 graphics 元素。
//
// 开标签的结束位置按**引号感知**扫描（属性值里可能有 '>'），子元素按
// `</graphics>` 收尾（graphics 不会嵌套，首个闭标签即匹配）。
// 结构损坏时返回 false——宁可不改，也不要拼出一份更坏的定义。
func findGraphicsElement(xmlText, protocol string) (graphicsElement, bool) {
	offset := 0
	for offset < len(xmlText) {
		i := strings.Index(xmlText[offset:], "<graphics")
		if i < 0 {
			return graphicsElement{}, false
		}
		start := offset + i
		after := start + len("<graphics")
		// 必须是 <graphics 后接空白或 '>'，避免匹配 <graphicsfoo
		if after < len(xmlText) && !isSpaceOrGT(xmlText[after]) {
			offset = after
			continue
		}
		openEnd, ok := xmlTagEnd(xmlText, start)
		if !ok {
			return graphicsElement{}, false
		}
		openTag := xmlText[start:openEnd]
		offset = openEnd
		if attrValue(openTag, "type") != protocol {
			continue
		}

		el := graphicsElement{start: start, openEnd: openEnd, openTag: openTag}
		if strings.HasSuffix(strings.TrimSpace(openTag), "/>") {
			el.end = openEnd
			return el, true
		}
		j := strings.Index(xmlText[openEnd:], "</graphics>")
		if j < 0 {
			return graphicsElement{}, false
		}
		el.end = openEnd + j + len("</graphics>")
		return el, true
	}
	return graphicsElement{}, false
}

// isSpaceOrGT 判断字符是否是标签名后的分隔符。
func isSpaceOrGT(c byte) bool {
	return c == '>' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '/'
}

// xmlTagEnd 返回从 start 起的标签结束下标（'>' 之后），跳过引号内的内容。
func xmlTagEnd(s string, start int) (int, bool) {
	var quote byte
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '>':
			return i + 1, true
		}
	}
	return 0, false
}

// syncListenChild 把 graphics 子元素里的 <listen type='address' address='…'/>
// 同步成目标地址。
//
// 为什么必须同步：libvirt 的 dumpxml 会同时写出 `listen='…'` **属性**和
// `<listen>` **子元素**，而子元素的优先级更高。只改属性的话，界面上"对外暴露"
// 显示成功、实际监听地址没变——一个安全相关的静默失效。
func syncListenChild(inner, listen string) string {
	i := strings.Index(inner, "<listen")
	if i < 0 {
		return inner
	}
	end := strings.IndexByte(inner[i:], '>')
	if end < 0 {
		return inner
	}
	tag := inner[i : i+end+1]
	newTag := setTagAttr(tag, "address", listen)
	if newTag == tag {
		return inner
	}
	return inner[:i] + newTag + inner[i+end+1:]
}

// applyConsoleGraphics 在域定义上应用一次控制台配置。
//
// enabled=false 删除该协议的 graphics 元素；true 时确保元素存在并更新
// 监听地址、端口与密码。返回 changed=false 表示这次变更在 XML 上没有
// 产生任何差异（幂等调用不该反复 define）。
func applyConsoleGraphics(
	xmlText, protocol string, enabled bool, listen string, port int, password string,
) (string, bool) {
	el, found := findGraphicsElement(xmlText, protocol)

	if !enabled {
		if !found {
			return xmlText, false
		}
		// 连同缩进与换行一起删掉：只删标签会留下空行，
		// 下次 dumpxml 里多出一段空白，diff 会把它当成一次改动。
		lineStart := el.start
		for lineStart > 0 && (xmlText[lineStart-1] == ' ' || xmlText[lineStart-1] == '\t') {
			lineStart--
		}
		if lineStart > 0 && xmlText[lineStart-1] == '\n' {
			lineStart--
		}
		removeEnd := el.end
		if removeEnd < len(xmlText) && xmlText[removeEnd] == '\n' {
			removeEnd++
		}
		return xmlText[:lineStart] + xmlText[removeEnd:], true
	}

	if !found {
		// 元素不存在时补一个：关闭控制台会把它删掉，重新开启必须能回来。
		fragment := "<graphics type='" + protocol + "'"
		if port > 0 {
			fragment += " port='" + strconv.Itoa(port) + "' autoport='no'"
		} else {
			fragment += " port='-1' autoport='yes'"
		}
		fragment += " listen='" + listen + "'"
		if password != "" {
			fragment += " passwd='" + xmlEsc(password) + "'"
		}
		fragment += "/>"
		i := strings.Index(xmlText, "</devices>")
		if i < 0 {
			return xmlText, false
		}
		return xmlText[:i] + "    " + fragment + "\n  " + xmlText[i:], true
	}

	// **只改开标签**：子元素（<listen>）原样保留，再单独同步它的地址。
	newTag := setTagAttr(el.openTag, "listen", listen)
	if port > 0 {
		newTag = setTagAttr(newTag, "autoport", "no")
		newTag = setTagAttr(newTag, "port", strconv.Itoa(port))
	} else {
		// 端口交给 libvirt 分配：控制面没有端口时硬塞 5900 会让同宿主机上
		// 的第二台机器起不来，而那是一个"保存成功、开机失败"的坑。
		newTag = setTagAttr(newTag, "autoport", "yes")
		newTag = setTagAttr(newTag, "port", "-1")
	}
	if password != "" {
		newTag = setTagAttr(newTag, "passwd", xmlEsc(password))
	}

	inner := xmlText[el.openEnd:el.end]
	newInner := syncListenChild(inner, listen)
	if newTag == el.openTag && newInner == inner {
		return xmlText, false
	}
	return xmlText[:el.start] + newTag + newInner + xmlText[el.end:], true
}

// elementText 取 <name>…</name> 的文本（用于域 XML 的域名校验）。
func elementText(xml, name string) string {
	start, end := "<"+name+">", "</"+name+">"
	i := strings.Index(xml, start)
	if i < 0 {
		return ""
	}
	rest := xml[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// removeSelfClosing 删除块内所有以 prefix 开头的自闭合标签（连同其缩进）。
func removeSelfClosing(block, prefix string) string {
	for {
		i := strings.Index(block, prefix)
		if i < 0 {
			return block
		}
		end := strings.Index(block[i:], "/>")
		if end < 0 {
			return block
		}
		start := i
		for start > 0 && (block[start-1] == ' ' || block[start-1] == '\t') {
			start--
		}
		if start > 0 && block[start-1] == '\n' {
			start--
		}
		block = block[:start] + block[i+end+2:]
	}
}

// setDiskBus 修改指定设备的目标总线（换总线需要停机，由控制面保证）。
func setDiskBus(xml, dev, bus string) (string, error) {
	for _, block := range xmlBlocks(xml, "<disk ", "</disk>") {
		disks := sys.ParseDomainDisks(block)
		if len(disks) == 0 || disks[0].Dev != dev {
			continue
		}
		i := strings.Index(block, "<target")
		if i < 0 {
			return "", fmt.Errorf("磁盘 %s 的定义缺少 <target>", dev)
		}
		end := strings.IndexByte(block[i:], '>')
		if end < 0 {
			return "", fmt.Errorf("磁盘 %s 的 <target> 不是完整标签", dev)
		}
		newTag := setTagAttr(block[i:i+end+1], "bus", bus)
		newBlock := block[:i] + newTag + block[i+end+1:]
		// 换总线后旧的 PCI 地址不再成立（virtio 与 scsi 的控制器不同）。
		// 不删掉地址，libvirt define 会报 "address not available" 或把
		// 设备挂到错误的控制器下；删掉后由它重新分配。
		newBlock = removeSelfClosing(newBlock, "<address")
		return strings.Replace(xml, block, newBlock, 1), nil
	}
	return "", fmt.Errorf("域定义里没有设备 %s", dev)
}

// setDiskSource 修改指定设备的镜像路径（磁盘迁移用）。
func setDiskSource(xml, dev, source string) (string, error) {
	for _, block := range xmlBlocks(xml, "<disk ", "</disk>") {
		disks := sys.ParseDomainDisks(block)
		if len(disks) == 0 || disks[0].Dev != dev {
			continue
		}

		var newBlock string
		if i := strings.Index(block, "<source"); i >= 0 {
			end := strings.IndexByte(block[i:], '>')
			if end < 0 {
				return "", fmt.Errorf("磁盘 %s 的 <source> 不是完整标签", dev)
			}
			tag := block[i : i+end+1]
			if attrValue(tag, "file") == "" {
				return "", fmt.Errorf("磁盘 %s 不是文件盘，无法迁移", dev)
			}
			newBlock = block[:i] + setTagAttr(tag, "file", xmlEsc(source)) + block[i+end+1:]
		} else {
			// 空光驱之类的无 source 设备不在迁移范围内；能走到这里说明
			// 调用方选错了设备，直接拒绝而不是插一个 source 改变设备语义。
			return "", fmt.Errorf("磁盘 %s 没有 source，无法迁移", dev)
		}
		return strings.Replace(xml, block, newBlock, 1), nil
	}
	return "", fmt.Errorf("域定义里没有设备 %s", dev)
}

// interfaceBlockOf 按 MAC 找到网卡设备块。
//
// 用 MAC 而不是序号定位：序号只存在于控制面，域 XML 里没有它；而 MAC
// 是控制面创建时固定下来的，重试与重复下发都能对上同一块网卡。
func interfaceBlockOf(xml, mac string) (string, bool) {
	if mac == "" {
		return "", false
	}
	for _, block := range xmlBlocks(xml, "<interface ", "</interface>") {
		if strings.EqualFold(attrOfTag(block, "mac", "address"), mac) {
			return block, true
		}
	}
	return "", false
}

// diskFragment 渲染一块可挂载的磁盘设备。
func diskFragment(dev, bus, source, format string) string {
	if format == "" {
		format = "qcow2"
	}
	return "<disk type='file' device='disk'>\n" +
		"      <driver name='qemu' type='" + xmlEsc(format) + "'/>\n" +
		"      <source file='" + xmlEsc(source) + "'/>\n" +
		"      <target dev='" + xmlEsc(dev) + "' bus='" + xmlEsc(bus) + "'/>\n" +
		"    </disk>"
}

// ifaceFragment 渲染一块网卡设备。
//
// bridge 为空表示使用 libvirt 用户态网络（创建时的默认值）；rateMbps > 0
// 时写入入向限速（libvirt 的 average 单位是 kbps）。
func ifaceFragment(model, mac, bridge string, rateMbps int) string {
	if model == "" {
		model = "virtio"
	}
	var b strings.Builder
	if bridge == "" {
		b.WriteString("<interface type='user'>\n")
	} else {
		b.WriteString("<interface type='bridge'>\n")
		b.WriteString("      <source bridge='" + xmlEsc(bridge) + "'/>\n")
	}
	if mac != "" {
		b.WriteString("      <mac address='" + xmlEsc(mac) + "'/>\n")
	}
	b.WriteString("      <model type='" + xmlEsc(model) + "'/>\n")
	if rateMbps > 0 {
		b.WriteString(fmt.Sprintf("      <bandwidth><inbound average='%d'/></bandwidth>\n", rateMbps*1000))
	}
	b.WriteString("    </interface>")
	return b.String()
}

// hasSCSIController 报告域里是否已有 SCSI 控制器。
func hasSCSIController(xml string) bool {
	return strings.Contains(xml, "<controller type='scsi'") ||
		strings.Contains(xml, `<controller type="scsi"`)
}

// addSCSIController 在 </devices> 前补一个 virtio-scsi 控制器。
//
// 创建时只有用了 scsi 盘的域才会生成控制器；给一台纯 virtio 的虚拟机
// 热挂 scsi 盘（q35 槽位耗尽时的降级路径）必须先有控制器。
func addSCSIController(xml string) string {
	if hasSCSIController(xml) {
		return xml
	}
	i := strings.LastIndex(xml, "</devices>")
	if i < 0 {
		return xml
	}
	return xml[:i] + "    <controller type='scsi' index='0' model='virtio-scsi'/>\n  " + xml[i:]
}

// hasFDCController 报告域里是否已有软驱控制器。
func hasFDCController(xml string) bool {
	return strings.Contains(xml, "<controller type='fdc'") ||
		strings.Contains(xml, `<controller type="fdc"`)
}

// addFDCController 在 </devices> 前补一个软驱控制器。
func addFDCController(xml string) string {
	if hasFDCController(xml) {
		return xml
	}
	i := strings.LastIndex(xml, "</devices>")
	if i < 0 {
		return xml
	}
	return xml[:i] + "    <controller type='fdc' index='0'/>\n  " + xml[i:]
}

// devPrefixForBus 返回某总线要求的数据盘设备名前缀。
//
// 空串表示"这种总线没有固定前缀"（例如 usb/sata 上的可移动介质），
// 此时调用方不做前缀校验。
func devPrefixForBus(bus string) string {
	switch bus {
	case "virtio":
		return "vd"
	case "scsi", "sata":
		return "sd"
	case "ide":
		return "hd"
	}
	return ""
}

// nextFreeDevForBus 按总线类型找第一个空闲的数据盘设备名。
//
// 从 b 开始：a 留给系统盘/光驱（创建时的第一块设备），从 a 开始会让新盘
// 与既有设备抢同一个名字。
func nextFreeDevForBus(xml, bus string) string {
	prefix := "vd"
	switch bus {
	case "scsi", "sata":
		prefix = "sd"
	case "ide":
		prefix = "hd"
	}
	used := map[string]bool{}
	for _, d := range sys.ParseDomainDisks(xml) {
		used[d.Dev] = true
	}
	for c := byte('b'); c <= 'z'; c++ {
		dev := prefix + string(c)
		if !used[dev] {
			return dev
		}
	}
	return prefix + "z"
}
