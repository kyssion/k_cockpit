package nodeagent

import (
	"encoding/xml"
	"strings"
	"testing"
)

// 本文件覆盖第 3 轮 DEF-R3-16：域 XML 从未写入 QEMU guest agent 通道。
//
// 真机证据：`AGENT_CHANNEL_IN_XML channels=0 guest_agent_target=0`，
// `virsh` 原文 `argument unsupported: QEMU guest agent is not configured`。
// 后果：来宾内改密、读 IP、就绪探测、自动弹介质全部不可达。

// minimalSpec 造一份最小可用的创建规格（无数据盘、无 UEFI）。
func minimalSpec() createSpec {
	return createSpec{
		Name: "kc-agent-probe", VCPU: 2, MemoryMB: 2048,
		Arch: "aarch64", Machine: "virt-8.2", NicModel: "virtio",
		RTCMode: "utc", DiskBus: "virtio", DiskFormat: "qcow2",
		SystemDiskPath: "/var/lib/libvirt/images/kc-agent-probe.qcow2",
	}
}

// TestBuildDomainXMLHasGuestAgentChannel 覆盖**创建**路径。
func TestBuildDomainXMLHasGuestAgentChannel(t *testing.T) {
	out := minimalSpec().BuildDomainXML()

	if !strings.Contains(out, guestAgentChannelName) {
		t.Fatalf("域定义里没有 guest agent 通道（DEF-R3-16 会复现）:\n%s", out)
	}
	if !strings.Contains(out, "<controller type='virtio-serial' index='0'/>") {
		t.Errorf("缺少 virtio-serial 控制器（通道没有载体）:\n%s", out)
	}
	if !strings.Contains(out, "<source mode='bind'/>") {
		t.Errorf("通道应带 <source mode='bind'/>（路径交给 libvirt 分配）:\n%s", out)
	}
	if n := strings.Count(out, "<controller type='virtio-serial'"); n != 1 {
		t.Errorf("virtio-serial 控制器应恰好 1 个，实际 %d", n)
	}
	if n := strings.Count(out, guestAgentChannelName); n != 1 {
		t.Errorf("agent 通道应恰好 1 个，实际 %d", n)
	}
	// 生成的必须是一份**合法 XML**（标签成对、无未转义字符）。
	var probe struct {
		XMLName xml.Name
		Devices struct {
			Channels []struct {
				Type   string `xml:"type,attr"`
				Target struct {
					Name string `xml:"name,attr"`
				} `xml:"target"`
			} `xml:"channel"`
		} `xml:"devices"`
	}
	if err := xml.Unmarshal([]byte(out), &probe); err != nil {
		t.Fatalf("生成的域定义不是合法 XML: %v\n%s", err, out)
	}
	if len(probe.Devices.Channels) != 1 {
		t.Fatalf("解析到 %d 个 channel，期望 1", len(probe.Devices.Channels))
	}
	ch := probe.Devices.Channels[0]
	if ch.Type != "unix" || ch.Target.Name != guestAgentChannelName {
		t.Errorf("通道形态不对: type=%q target=%q", ch.Type, ch.Target.Name)
	}
}

// dumpWithoutChannel 是"纳管/手工编辑"场景下的真实形状：virsh dumpxml 输出，
// 没有 channel（老域就是这样）。
const dumpWithoutChannel = `<domain type='kvm'>
  <name>legacy</name>
  <memory unit='KiB'>2097152</memory>
  <devices>
    <disk type='file' device='disk'>
      <source file='/var/lib/libvirt/images/legacy.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <interface type='bridge'>
      <source bridge='br-ovs'/>
    </interface>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>
    <console type='pty'/>
  </devices>
</domain>
`

// TestEnsureGuestAgentChannelOnDumpXML 覆盖**纳管**与**改 XML** 两条路径。
func TestEnsureGuestAgentChannelOnDumpXML(t *testing.T) {
	patched, changed := ensureGuestAgentChannel(dumpWithoutChannel)
	if !changed {
		t.Fatal("没有通道时应报告已改动")
	}
	if !hasGuestAgentChannel(patched) {
		t.Fatalf("补齐后仍没有通道:\n%s", patched)
	}
	if !hasVirtioSerialController(patched) {
		t.Errorf("补齐后缺少 virtio-serial 控制器:\n%s", patched)
	}
	// 必须落在 </devices> 之前、且其它设备一个不少。
	if strings.Index(patched, guestAgentChannelName) > strings.Index(patched, "</devices>") {
		t.Error("通道插到了 </devices> 之后，域定义会变成非法")
	}
	for _, keep := range []string{"<disk type='file' device='disk'>", "<interface type='bridge'>",
		"<graphics type='vnc'", "<console type='pty'/>"} {
		if !strings.Contains(patched, keep) {
			t.Errorf("补齐通道时弄丢了既有设备 %q", keep)
		}
	}
	// 结构必须仍然合法。
	var probe struct {
		XMLName xml.Name
	}
	if err := xml.Unmarshal([]byte(patched), &probe); err != nil {
		t.Fatalf("补齐通道后不是合法 XML: %v\n%s", err, patched)
	}

	// 幂等：再补一次不应改动（否则每次 define 都会产生一次无谓写入）。
	if again, changedAgain := ensureGuestAgentChannel(patched); changedAgain || again != patched {
		t.Error("已有通道时应原样返回（幂等）")
	}
}

// TestEnsureGuestAgentChannelKeepsSingleController 覆盖"有控制器、没通道"。
func TestEnsureGuestAgentChannelKeepsSingleController(t *testing.T) {
	base := strings.Replace(dumpWithoutChannel, "<console type='pty'/>",
		"<controller type='virtio-serial' index='0'/>\n    <console type='pty'/>", 1)
	patched, changed := ensureGuestAgentChannel(base)
	if !changed {
		t.Fatal("没有通道时应报告已改动")
	}
	if n := strings.Count(patched, "<controller type='virtio-serial'"); n != 1 {
		t.Errorf("控制器不应重复添加，实际 %d 个", n)
	}
	if !hasGuestAgentChannel(patched) {
		t.Error("通道仍未补上")
	}
}

// TestEnsureGuestAgentChannelIgnoresBrokenXML 覆盖结构不认识时的行为。
//
// 不是标准域定义（没有 </devices>）时**不改**：拼一份更坏的定义比没有通道更糟，
// 调用方（纳管）会拿到 changed=false 并如实告知用户。
func TestEnsureGuestAgentChannelIgnoresBrokenXML(t *testing.T) {
	if out, changed := ensureGuestAgentChannel("<domain><name>x</name></domain>"); changed || out != "<domain><name>x</name></domain>" {
		t.Error("结构不认识时不应改动")
	}
}

// TestGuestAgentChannelSameInAllPaths 覆盖"三条路径产出同一份通道"。
func TestGuestAgentChannelSameInAllPaths(t *testing.T) {
	created := minimalSpec().BuildDomainXML()
	adopted, _ := ensureGuestAgentChannel(dumpWithoutChannel)
	fromUserXML, _ := ensureGuestAgentChannel(strings.Replace(dumpWithoutChannel, "legacy", "hand-edited", 1))

	block := func(s string) string {
		i := strings.Index(s, "<channel type='unix'>")
		if i < 0 {
			return ""
		}
		j := strings.Index(s[i:], "</channel>")
		if j < 0 {
			return ""
		}
		return s[i : i+j+len("</channel>")]
	}
	want := block(created)
	if want == "" {
		t.Fatal("创建路径没有通道块")
	}
	for name, got := range map[string]string{"adopt": block(adopted), "xml-apply": block(fromUserXML)} {
		if got != want {
			t.Errorf("%s 路径的通道块与创建路径不一致:\n got=%q\nwant=%q", name, got, want)
		}
	}
}

// 网卡必须落到所选网桥上；OVS 桥还要带 virtualport。
//
// 实机抓出的两个缺陷都在这一段：
//  1. 创建时网卡恒为 type='user'（SLIRP），选的交换机只写进了数据库——
//     端口转发 / 安全组 / DHCP 对新建虚拟机全部不生效；
//  2. 接 OVS 桥但不声明 virtualport 时，libvirt 按 Linux 桥处理，
//     开机报 "Unable to add bridge kbrX port vnet0: Operation not supported"。
func TestBuildDomainXMLNetworkAttachment(t *testing.T) {
	spec := minimalSpec()

	spec.NicBridge = "kbr0"
	out := spec.BuildDomainXML()
	if !strings.Contains(out, "<interface type='bridge'>") || !strings.Contains(out, "<source bridge='kbr0'/>") {
		t.Fatalf("Linux 桥未写进域定义:\n%s", out)
	}
	if strings.Contains(out, "virtualport") {
		t.Errorf("Linux 桥不应带 virtualport:\n%s", out)
	}

	spec.NicBridgeOVS = true
	out = spec.BuildDomainXML()
	if !strings.Contains(out, "<virtualport type='openvswitch'/>") {
		t.Fatalf("OVS 桥缺少 virtualport（开机会失败）:\n%s", out)
	}

	spec.NicBridge = ""
	spec.NicBridgeOVS = false
	if out = spec.BuildDomainXML(); !strings.Contains(out, "<interface type='user'>") {
		t.Errorf("无网桥时应退回用户态网络:\n%s", out)
	}
}
