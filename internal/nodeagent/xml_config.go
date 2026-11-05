// xml_config.go：把 vm.config.update 中**可落到域定义**的项写进 XML。
//
// 与 xml_edit.go 同样的取舍：字符串级定点修改，不做整份反序列化再序列化——
// 后者会把用户没动过的部分重排，diff 与审计都失去意义。
//
// 这里只处理「改元素/属性就能生效」的项。机型、固件、架构这类必须重建域的
// 项不在此列，由控制面在受理阶段拒绝（见 agent.AppliedConfigKeys）。
package nodeagent

import (
	"fmt"
	"strconv"
	"strings"
)

// xmlConfigKeys 列出本次变更里属于「改域定义」的键。
func xmlConfigKeys(changes map[string]any) []string {
	candidates := []string{
		"boot_order", "video_model", "rtc_mode", "watchdog",
		"apic", "pae", "hide_kvm", "memory_hugepages",
		"cpu_sockets", "cpu_cores", "cpu_threads",
		"cpu_affinity", "cpu_limit_percent",
	}
	out := make([]string, 0, len(candidates))
	for _, key := range candidates {
		if _, ok := changes[key]; ok {
			out = append(out, key)
		}
	}
	return out
}

// applyConfigXML 应用一批 XML 类配置，返回新 XML 与真正改动的键。
func applyConfigXML(xml string, changes map[string]any) (string, []string) {
	var applied []string
	set := func(key string, fn func(string) (string, bool)) {
		out, ok := fn(xml)
		if ok {
			xml = out
			applied = append(applied, key)
		}
	}

	if v, ok := strField(changes, "boot_order"); ok {
		set("boot_order", func(x string) (string, bool) { return setBootOrder(x, v) })
	}
	if v, ok := strField(changes, "video_model"); ok {
		set("video_model", func(x string) (string, bool) { return setVideoModel(x, v) })
	}
	if v, ok := strField(changes, "rtc_mode"); ok {
		set("rtc_mode", func(x string) (string, bool) { return setClockOffset(x, v) })
	}
	if v, ok := strField(changes, "watchdog"); ok {
		set("watchdog", func(x string) (string, bool) { return setWatchdog(x, v) })
	}
	if v, ok := boolField(changes, "apic"); ok {
		set("apic", func(x string) (string, bool) { return setFeatureFlag(x, "apic", v) })
	}
	if v, ok := boolField(changes, "pae"); ok {
		set("pae", func(x string) (string, bool) { return setFeatureFlag(x, "pae", v) })
	}
	if v, ok := boolField(changes, "hide_kvm"); ok {
		set("hide_kvm", func(x string) (string, bool) { return setKVMHidden(x, v) })
	}
	if v, ok := boolField(changes, "memory_hugepages"); ok {
		set("memory_hugepages", func(x string) (string, bool) { return setMemoryHugepages(x, v) })
	}

	// CPU 拓扑：三个维度必须一起提交（控制面在受理时补齐），否则改了也会
	// 因为缺少另外两个值而无法生成合法的 <topology/>。
	sockets, okS := numField(changes, "cpu_sockets")
	cores, okC := numField(changes, "cpu_cores")
	threads, okT := numField(changes, "cpu_threads")
	if okS || okC || okT {
		set("cpu_sockets/cpu_cores/cpu_threads", func(x string) (string, bool) {
			return setCPUTopology(x, sockets, cores, threads)
		})
	}

	// CPU 配额与绑核共用一个 <cputune>：分两次替换会互相覆盖。
	quota := existingCPUQuota(xml)
	cpuset := existingCPUSet(xml)
	tuneChanged := false
	if v, ok := numField(changes, "cpu_limit_percent"); ok {
		if v <= 0 {
			quota = ""
		} else {
			// libvirt 缺省 vCPU 数为 1：没有 <vcpu> 时按 1 计算，
			// 否则配额会被算成 0（等于"不限"），与用户的设置相反。
			vcpu := vcpuCountOf(xml)
			if vcpu <= 0 {
				vcpu = 1
			}
			quota = strconv.Itoa(v * vcpu * 100)
		}
		tuneChanged = true
	}
	if v, ok := strField(changes, "cpu_affinity"); ok {
		cpuset = strings.TrimSpace(v)
		tuneChanged = true
	}
	if tuneChanged {
		set("cpu_limit_percent/cpu_affinity", func(x string) (string, bool) {
			vcpu := vcpuCountOf(x)
			if vcpu <= 0 {
				vcpu = 1
			}
			return applyCPUTune(x, quota, cpuset, vcpu)
		})
	}

	return xml, applied
}

// --- 引导顺序 ---

// bootDevName 把界面口径的磁盘/网卡名翻译成 libvirt 的 boot dev 取值。
//
// 域 XML 只认 fd / hd / cdrom / network；直接写 "disk" 会被 define 拒绝。
// 创建与编辑共用这一处，避免两条路径各写一份映射。
func bootDevName(dev string) string {
	switch strings.ToLower(strings.TrimSpace(dev)) {
	case "disk", "hd", "hdd", "vda":
		return "hd"
	case "cdrom", "cd", "cd-rom":
		return "cdrom"
	case "network", "net", "pxe":
		return "network"
	case "fd", "floppy":
		return "fd"
	}
	return ""
}

func setBootOrder(xml, order string) (string, bool) {
	i := strings.Index(xml, "<os>")
	if i < 0 {
		return xml, false
	}
	j := strings.Index(xml[i:], "</os>")
	if j < 0 {
		return xml, false
	}
	end := i + j + len("</os>")
	block := xml[i:end]

	newBlock := removeSelfClosing(block, "<boot")
	var frag strings.Builder
	for _, dev := range strings.Split(order, ",") {
		if d := bootDevName(dev); d != "" {
			frag.WriteString("    <boot dev='" + d + "'/>\n")
		}
	}
	if frag.Len() > 0 {
		k := strings.Index(newBlock, "</os>")
		newBlock = newBlock[:k] + frag.String() + newBlock[k:]
	}
	if newBlock == block {
		return xml, false
	}
	return xml[:i] + newBlock + xml[end:], true
}

// --- 显示设备 ---

func setVideoModel(xml, model string) (string, bool) {
	i := strings.Index(xml, "<video>")
	j := -1
	if i >= 0 {
		j = strings.Index(xml[i:], "</video>")
	}
	if i >= 0 && j >= 0 {
		end := i + j + len("</video>")
		block := xml[i:end]
		if model == "none" {
			return removeBlockWithIndent(xml, i, end), true
		}
		k := strings.Index(block, "<model")
		if k < 0 {
			newBlock := block[:len("<video>")] + "\n      <model type='" + xmlEsc(model) + "'/>" + block[len("<video>"):]
			return xml[:i] + newBlock + xml[end:], true
		}
		mEnd := strings.Index(block[k:], "/>")
		if mEnd < 0 {
			return xml, false
		}
		newTag := setTagAttr(block[k:k+mEnd+2], "type", model)
		newBlock := block[:k] + newTag + block[k+mEnd+2:]
		if newBlock == block {
			return xml, false
		}
		return xml[:i] + newBlock + xml[end:], true
	}
	if model == "none" {
		return xml, false
	}
	return insertBeforeDevices(xml, "    <video><model type='"+xmlEsc(model)+"'/></video>\n"), true
}

// --- RTC 时钟 ---

func setClockOffset(xml, mode string) (string, bool) {
	start, end, tag, ok := findSelfClosing(xml, "<clock")
	if ok {
		newTag := setTagAttr(tag, "offset", mode)
		if newTag == tag {
			return xml, false
		}
		return xml[:start] + newTag + xml[end:], true
	}
	return insertBeforeDevices(xml, "  <clock offset='"+xmlEsc(mode)+"'/>\n"), true
}

// --- 特性开关 ---

func setFeatureFlag(xml, name string, on bool) (string, bool) {
	start, end, ok := featuresBlock(xml)
	if !ok {
		if !on {
			return xml, false
		}
		frag := "  <features>\n    <" + name + "/>\n  </features>\n"
		return insertBeforeDevices(xml, frag), true
	}
	block := xml[start:end]
	has := strings.Contains(block, "<"+name)
	switch {
	case on && !has:
		k := strings.Index(block, "</features>")
		newBlock := block[:k] + "    <" + name + "/>\n  " + block[k:]
		return xml[:start] + newBlock + xml[end:], true
	case !on && has:
		newBlock := removeSelfClosing(block, "<"+name)
		if newBlock == block {
			return xml, false
		}
		return xml[:start] + newBlock + xml[end:], true
	}
	return xml, false
}

func setKVMHidden(xml string, on bool) (string, bool) {
	start, end, ok := featuresBlock(xml)
	if !ok {
		if !on {
			return xml, false
		}
		return insertBeforeDevices(xml,
			"  <features>\n    <kvm><hidden state='on'/></kvm>\n  </features>\n"), true
	}
	block := xml[start:end]
	stripped := removeElementBlock(block, "<kvm", "</kvm>")
	stripped = removeSelfClosing(stripped, "<kvm")
	if !on {
		if stripped == block {
			return xml, false
		}
		return xml[:start] + stripped + xml[end:], true
	}
	k := strings.Index(stripped, "</features>")
	newBlock := stripped[:k] + "    <kvm><hidden state='on'/></kvm>\n  " + stripped[k:]
	return xml[:start] + newBlock + xml[end:], true
}

func setMemoryHugepages(xml string, on bool) (string, bool) {
	i := strings.Index(xml, "<memoryBacking>")
	if i >= 0 {
		j := strings.Index(xml[i:], "</memoryBacking>")
		if j >= 0 {
			end := i + j + len("</memoryBacking>")
			if !on {
				return removeBlockWithIndent(xml, i, end), true
			}
			return xml, false
		}
	}
	if !on {
		return xml, false
	}
	return insertBeforeDevices(xml, "  <memoryBacking>\n    <hugepages/>\n  </memoryBacking>\n"), true
}

// --- 看门狗 ---

func setWatchdog(xml, action string) (string, bool) {
	start, end, tag, found := findSelfClosing(xml, "<watchdog")
	if action == "" || action == "none" {
		if !found {
			return xml, false
		}
		return removeBlockWithIndent(xml, start, end), true
	}
	if found {
		newTag := setTagAttr(tag, "action", action)
		if newTag == tag {
			return xml, false
		}
		return xml[:start] + newTag + xml[end:], true
	}
	frag := "    <watchdog model='i6300esb' action='" + xmlEsc(action) + "'/>\n"
	return insertBeforeDevices(xml, frag), true
}

// --- CPU 拓扑 ---

func setCPUTopology(xml string, sockets, cores, threads int) (string, bool) {
	var topo string
	if sockets > 0 && cores > 0 && threads > 0 {
		topo = fmt.Sprintf("    <topology sockets='%d' cores='%d' threads='%d'/>\n", sockets, cores, threads)
	}

	start, end, tag, selfClosing := findSelfClosing(xml, "<cpu ")
	if selfClosing {
		if topo == "" {
			return xml, false
		}
		open := strings.TrimSuffix(tag, "/>") + ">"
		return xml[:start] + open + "\n" + topo + "  </cpu>" + xml[end:], true
	}

	cs := strings.Index(xml, "<cpu ")
	if cs < 0 {
		cs = strings.Index(xml, "<cpu>")
	}
	if cs < 0 {
		return xml, false
	}
	closeAt := strings.Index(xml[cs:], "</cpu>")
	if closeAt < 0 {
		return xml, false
	}
	ce := cs + closeAt + len("</cpu>")
	block := xml[cs:ce]
	stripped := removeSelfClosing(block, "<topology")
	if topo != "" {
		k := strings.Index(stripped, "</cpu>")
		stripped = stripped[:k] + topo + "  " + stripped[k:]
	}
	if stripped == block {
		return xml, false
	}
	return xml[:cs] + stripped + xml[ce:], true
}

// --- CPU 配额与绑核（共用 cputune）---

func applyCPUTune(xml, quota, cpuset string, vcpuCount int) (string, bool) {
	var children strings.Builder
	if quota != "" {
		children.WriteString("    <quota>" + quota + "</quota>\n")
	}
	if cpuset != "" && vcpuCount > 0 {
		for i := 0; i < vcpuCount; i++ {
			children.WriteString(fmt.Sprintf("    <vcpupin vcpu='%d' cpuset='%s'/>\n", i, cpuset))
		}
	}

	i := strings.Index(xml, "<cputune>")
	if i >= 0 {
		j := strings.Index(xml[i:], "</cputune>")
		if j >= 0 {
			end := i + j + len("</cputune>")
			if children.Len() == 0 {
				return removeBlockWithIndent(xml, i, end), true
			}
			return xml[:i] + "<cputune>\n" + children.String() + "  </cputune>" + xml[end:], true
		}
	}
	if children.Len() == 0 {
		return xml, false
	}
	return insertBeforeDevices(xml, "  <cputune>\n"+children.String()+"  </cputune>\n"), true
}

// --- 通用小助手 ---

func strField(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

// findSelfClosing 定位以 prefix 开头的自闭合标签。
func findSelfClosing(xml, prefix string) (start, end int, tag string, ok bool) {
	i := strings.Index(xml, prefix)
	if i < 0 {
		return -1, -1, "", false
	}
	j := strings.Index(xml[i:], "/>")
	if j < 0 {
		return -1, -1, "", false
	}
	return i, i + j + 2, xml[i : i+j+2], true
}

// featuresBlock 返回 <features>…</features> 的范围（end 为开区间）。
func featuresBlock(xml string) (start, end int, ok bool) {
	i := strings.Index(xml, "<features>")
	if i < 0 {
		return -1, -1, false
	}
	j := strings.Index(xml[i:], "</features>")
	if j < 0 {
		return -1, -1, false
	}
	return i, i + j + len("</features>"), true
}

// insertBeforeDevices 把片段插到 <devices> 之前；没有 devices 时插到 </domain> 之前。
func insertBeforeDevices(xml, frag string) string {
	if i := strings.Index(xml, "<devices>"); i >= 0 {
		return xml[:i] + frag + xml[i:]
	}
	if i := strings.Index(xml, "</domain>"); i >= 0 {
		return xml[:i] + frag + xml[i:]
	}
	return xml + frag
}

// removeBlockWithIndent 删除 [start,end) 以及其所在行的缩进与换行。
func removeBlockWithIndent(xml string, start, end int) string {
	lineStart := start
	for lineStart > 0 && (xml[lineStart-1] == ' ' || xml[lineStart-1] == '\t') {
		lineStart--
	}
	if lineStart > 0 && xml[lineStart-1] == '\n' {
		lineStart--
	}
	removeEnd := end
	if removeEnd < len(xml) && xml[removeEnd] == '\n' {
		removeEnd++
	}
	return xml[:lineStart] + xml[removeEnd:]
}

// removeElementBlock 删除 openTag…closeTag 的整块（含缩进）。
func removeElementBlock(xml, openTag, closeTag string) string {
	i := strings.Index(xml, openTag)
	if i < 0 {
		return xml
	}
	j := strings.Index(xml[i:], closeTag)
	if j < 0 {
		return xml
	}
	return removeBlockWithIndent(xml, i, i+j+len(closeTag))
}

// vcpuCountOf 读取 <vcpu …>N</vcpu> 的数量。
func vcpuCountOf(xml string) int {
	i := strings.Index(xml, "<vcpu")
	if i < 0 {
		return 0
	}
	open := strings.Index(xml[i:], ">")
	if open < 0 {
		return 0
	}
	rest := xml[i+open+1:]
	j := strings.Index(rest, "</vcpu>")
	if j < 0 {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(rest[:j]))
	if err != nil {
		return 0
	}
	return n
}

// existingCPUQuota 读取现有 cputune/quota（没有则空）。
func existingCPUQuota(xml string) string {
	i := strings.Index(xml, "<cputune>")
	if i < 0 {
		return ""
	}
	j := strings.Index(xml[i:], "</cputune>")
	if j < 0 {
		return ""
	}
	block := xml[i : i+j]
	k := strings.Index(block, "<quota>")
	if k < 0 {
		return ""
	}
	rest := block[k+len("<quota>"):]
	e := strings.Index(rest, "</quota>")
	if e < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:e])
}

// existingCPUSet 读取现有 vcpupin 的第一个 cpuset（没有则空）。
func existingCPUSet(xml string) string {
	i := strings.Index(xml, "<vcpupin")
	if i < 0 {
		return ""
	}
	end := strings.Index(xml[i:], "/>")
	if end < 0 {
		return ""
	}
	return attrValue(xml[i:i+end+2], "cpuset")
}
