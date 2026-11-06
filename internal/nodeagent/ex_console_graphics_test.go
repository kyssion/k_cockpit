package nodeagent

import (
	"encoding/xml"
	"strings"
	"testing"
)

// 本文件覆盖第 3 轮 DEF-R3-18：改控制台口令时拼出标签错配的定义。
//
// 真机原文：`Opening and ending tag mismatch: devices line 26 and graphics
// </input> </graphics>`（SEC-02，改口令 100% 失败）。
//
// 根因：libvirt 的 graphics **带子元素** <listen/>，旧实现按第一个 "/>"
// 收尾，把子元素也当成标签的一部分；新增属性（passwd）因此被插到内层
// <listen/> 上，产出错配结构。

// consoleDump 是 virsh dumpxml 的真实形状（两段 graphics 都带 <listen/> 子元素）。
const consoleDump = `<?xml version='1.0' encoding='UTF-8'?>
<domain type='kvm'>
  <name>kc-console-probe</name>
  <devices>
    <disk type='file' device='disk'>
      <source file='/var/lib/libvirt/images/probe.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <input type='tablet' bus='usb'/>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <graphics type='spice' autoport='yes' listen='127.0.0.1'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <input type='mouse' bus='ps2'/>
    <console type='pty'/>
  </devices>
</domain>
`

// graphicsOf 解析出指定协议的 graphics 开标签与它的 listen 子元素地址。
func graphicsOf(t *testing.T, doc, protocol string) (map[string]string, string, bool) {
	t.Helper()
	type listenEl struct {
		Type    string `xml:"type,attr"`
		Address string `xml:"address,attr"`
	}
	type graphicsEl struct {
		Type   string     `xml:"type,attr"`
		Listen string     `xml:"listen,attr"`
		Port   string     `xml:"port,attr"`
		Auto   string     `xml:"autoport,attr"`
		Passwd string     `xml:"passwd,attr"`
		Kids   []listenEl `xml:"listen"`
	}
	var probe struct {
		XMLName xml.Name
		Devices struct {
			Graphics []graphicsEl `xml:"graphics"`
			Inputs   []struct {
				Type string `xml:"type,attr"`
			} `xml:"input"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(doc), &probe); err != nil {
		t.Fatalf("输出不是合法 XML: %v\n%s", err, doc)
	}
	for _, g := range probe.Devices.Graphics {
		if g.Type != protocol {
			continue
		}
		attrs := map[string]string{
			"listen": g.Listen, "port": g.Port, "autoport": g.Auto, "passwd": g.Passwd,
		}
		addr := ""
		if len(g.Kids) > 0 {
			addr = g.Kids[0].Address
		}
		return attrs, addr, true
	}
	return nil, "", false
}

// TestConsolePasswordChangeKeepsWellFormedXML 是本缺陷的主症状：
// 改口令必须产出一份能被 XML 解析器（以及 libvirt）接受的定义。
func TestConsolePasswordChangeKeepsWellFormedXML(t *testing.T) {
	out, changed := applyConsoleGraphics(consoleDump, "vnc", true, "127.0.0.1", 0, "S3cret-Pass")
	if !changed {
		t.Fatal("改口令应报告已改动")
	}
	// 1) 结构必须合法（旧实现在这里就崩）。
	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("改口令后不是合法 XML: %v\n%s", err, out)
	}
	// 2) 口令必须落在 graphics 开标签上，而不是 <listen/> 上。
	attrs, addr, ok := graphicsOf(t, out, "vnc")
	if !ok {
		t.Fatalf("找不到 vnc graphics:\n%s", out)
	}
	if attrs["passwd"] != "S3cret-Pass" {
		t.Errorf("passwd 应落在 graphics 元素上，实际 %q\n%s", attrs["passwd"], out)
	}
	if addr != "127.0.0.1" {
		t.Errorf("listen 子元素地址被破坏: %q", addr)
	}
	// 3) 标签成对：没有出现"多出来的闭标签"这种形状。
	if strings.Contains(out, "</input> </graphics>") || strings.Contains(out, "</listen> </graphics>") {
		t.Errorf("出现标签错配:\n%s", out)
	}
	if n := strings.Count(out, "</graphics>"); n != 2 {
		t.Errorf("</graphics> 数量 = %d，期望 2（两段 graphics 各一个）", n)
	}
	if n := strings.Count(out, "</devices>"); n != 1 {
		t.Errorf("</devices> 数量 = %d，期望 1", n)
	}
	// 4) 其它设备一个不少（input 两个）。
	if n := strings.Count(out, "<input "); n != 2 {
		t.Errorf("<input> 数量 = %d，期望 2", n)
	}
}

// TestConsoleListenChangeSyncsChildElement 覆盖"对外暴露"必须真的生效。
//
// libvirt 的 dumpxml 同时写 listen 属性与 <listen> 子元素，子元素优先。
// 只改属性的话，界面显示已暴露、实际监听地址没变——安全相关的静默失效。
func TestConsoleListenChangeSyncsChildElement(t *testing.T) {
	out, changed := applyConsoleGraphics(consoleDump, "vnc", true, "0.0.0.0", 5901, "")
	if !changed {
		t.Fatal("改监听地址应报告已改动")
	}
	attrs, addr, ok := graphicsOf(t, out, "vnc")
	if !ok {
		t.Fatal("找不到 vnc graphics")
	}
	if attrs["listen"] != "0.0.0.0" {
		t.Errorf("graphics listen = %q，期望 0.0.0.0", attrs["listen"])
	}
	if addr != "0.0.0.0" {
		t.Errorf("<listen> 子元素地址 = %q，期望同步为 0.0.0.0（否则实际监听不变）", addr)
	}
	if attrs["port"] != "5901" || attrs["autoport"] != "no" {
		t.Errorf("端口未按固定端口写入: port=%q autoport=%q", attrs["port"], attrs["autoport"])
	}
	// spice 那段不该被动过。
	spiceAttrs, spiceAddr, ok := graphicsOf(t, out, "spice")
	if !ok || spiceAttrs["listen"] != "127.0.0.1" || spiceAddr != "127.0.0.1" {
		t.Errorf("spice 配置被误改: %v %q", spiceAttrs, spiceAddr)
	}
}

// TestConsoleUpdateIdempotent 覆盖重复提交同一配置（不该反复 define）。
func TestConsoleUpdateIdempotent(t *testing.T) {
	first, changed := applyConsoleGraphics(consoleDump, "vnc", true, "127.0.0.1", 0, "pw-1")
	if !changed {
		t.Fatal("首次应改动")
	}
	second, changedAgain := applyConsoleGraphics(first, "vnc", true, "127.0.0.1", 0, "pw-1")
	if changedAgain || second != first {
		t.Error("同样的配置第二次不该产生改动")
	}
}

// TestConsoleDisableRemovesOnlyThatProtocol 覆盖关闭控制台。
func TestConsoleDisableRemovesOnlyThatProtocol(t *testing.T) {
	out, changed := applyConsoleGraphics(consoleDump, "vnc", false, "127.0.0.1", 0, "")
	if !changed {
		t.Fatal("关闭应改动")
	}
	if _, _, ok := graphicsOf(t, out, "vnc"); ok {
		t.Errorf("vnc 元素应被删除:\n%s", out)
	}
	if _, _, ok := graphicsOf(t, out, "spice"); !ok {
		t.Error("spice 不该被删")
	}
	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("删除后不是合法 XML: %v\n%s", err, out)
	}
}

// TestConsoleUpdateOnSelfClosingGraphics 覆盖旧形态（自闭合、无子元素）。
func TestConsoleUpdateOnSelfClosingGraphics(t *testing.T) {
	doc := strings.Replace(consoleDump, `>
      <listen type='address' address='127.0.0.1'/>
    </graphics>`, "/>", 1)
	out, changed := applyConsoleGraphics(doc, "vnc", true, "127.0.0.1", 0, "pw-2")
	if !changed {
		t.Fatal("自闭合元素也应能改")
	}
	attrs, _, ok := graphicsOf(t, out, "vnc")
	if !ok || attrs["passwd"] != "pw-2" {
		t.Errorf("自闭合元素改口令失败: %v\n%s", attrs, out)
	}
}

// TestFindGraphicsElementSkipsOtherProtocol 覆盖多段 graphics 的定位。
func TestFindGraphicsElementSkipsOtherProtocol(t *testing.T) {
	el, ok := findGraphicsElement(consoleDump, "spice")
	if !ok {
		t.Fatal("应能找到 spice 元素")
	}
	if attrValue(el.openTag, "type") != "spice" {
		t.Errorf("定位到了错误的元素: %q", el.openTag)
	}
	// 整段元素应包含它自己的 </graphics>（而不是被下一段吃掉）。
	seg := consoleDump[el.start:el.end]
	if !strings.HasSuffix(seg, "</graphics>") {
		t.Errorf("元素范围不对: %q", seg)
	}
	if !strings.Contains(seg, "type='spice'") || strings.Contains(seg, "type='vnc'") {
		t.Errorf("元素范围串到了别的协议: %q", seg)
	}
}

// TestFindGraphicsElementBrokenStructure 覆盖结构损坏时"宁可不改"。
func TestFindGraphicsElementBrokenStructure(t *testing.T) {
	broken := "<devices><graphics type='vnc' listen='127.0.0.1'></devices>"
	if _, ok := findGraphicsElement(broken, "vnc"); ok {
		t.Error("缺 </graphics> 时应判定为找不到（不改，交给 libvirt 拒绝）")
	}
}
