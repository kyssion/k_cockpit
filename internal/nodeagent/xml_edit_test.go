package nodeagent

import (
	"strings"
	"testing"
)

const testDomainXML = `<domain type='kvm'>
  <name>demo-vm</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/k_cockpit/images/demo-vm.qcow2'/>
      <target dev='vda' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x05' function='0x0'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/k_cockpit/userdata/7/data.qcow2'/>
      <target dev='vdb' bus='virtio'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x06' function='0x0'/>
    </disk>
    <interface type='bridge'>
      <source bridge='kbr0'/>
      <mac address='52:54:00:aa:bb:01'/>
      <model type='virtio'/>
    </interface>
  </devices>
</domain>`

func TestSetDiskBusOnlyTargetDevice(t *testing.T) {
	out, err := setDiskBus(testDomainXML, "vdb", "scsi")
	if err != nil {
		t.Fatalf("setDiskBus: %v", err)
	}
	if !strings.Contains(out, "<target dev='vdb' bus='scsi'/>") {
		t.Errorf("vdb 的总线未改成 scsi:\n%s", out)
	}
	if !strings.Contains(out, "<target dev='vda' bus='virtio'/>") {
		t.Errorf("vda 的总线被误改:\n%s", out)
	}
	if strings.Contains(out, "slot='0x06'") {
		t.Errorf("vdb 的旧 PCI 地址应被移除（换总线后不成立）:\n%s", out)
	}
	if !strings.Contains(out, "slot='0x05'") {
		t.Errorf("vda 的 PCI 地址被误删:\n%s", out)
	}
}

func TestSetDiskBusMissingDevice(t *testing.T) {
	if _, err := setDiskBus(testDomainXML, "vdz", "scsi"); err == nil {
		t.Fatal("对不存在的设备应返回错误")
	}
}

func TestSetDiskSourceOnlyTargetDevice(t *testing.T) {
	out, err := setDiskSource(testDomainXML, "vdb", "/var/lib/k_cockpit/pools/p2/data.qcow2")
	if err != nil {
		t.Fatalf("setDiskSource: %v", err)
	}
	if !strings.Contains(out, "file='/var/lib/k_cockpit/pools/p2/data.qcow2'") {
		t.Errorf("vdb 的 source 未替换:\n%s", out)
	}
	if !strings.Contains(out, "file='/var/lib/k_cockpit/images/demo-vm.qcow2'") {
		t.Errorf("vda 的 source 被误改:\n%s", out)
	}
}

func TestInterfaceBlockOfByMAC(t *testing.T) {
	block, ok := interfaceBlockOf(testDomainXML, "52:54:00:AA:BB:01")
	if !ok {
		t.Fatal("按 MAC 应能找到网卡（大小写不敏感）")
	}
	if !strings.Contains(block, "bridge='kbr0'") {
		t.Errorf("取到的网卡块不对:\n%s", block)
	}
	if _, ok := interfaceBlockOf(testDomainXML, "52:54:00:00:00:99"); ok {
		t.Fatal("不存在的 MAC 不应命中")
	}
}

func TestIfaceFragment(t *testing.T) {
	frag := ifaceFragment("virtio", "52:54:00:aa:bb:01", "kbr0", 100)
	for _, want := range []string{
		"type='bridge'", "bridge='kbr0'", "mac address='52:54:00:aa:bb:01'",
		"model type='virtio'", "average='100000'",
	} {
		if !strings.Contains(frag, want) {
			t.Errorf("网卡片段缺少 %q:\n%s", want, frag)
		}
	}

	user := ifaceFragment("e1000", "", "", 0)
	if !strings.Contains(user, "type='user'") || strings.Contains(user, "<source") {
		t.Errorf("无桥时应生成 user 模式且不带 source:\n%s", user)
	}
}

func TestDiskFragmentEscapes(t *testing.T) {
	frag := diskFragment("vdb", "virtio", "/data/a&b'c.qcow2", "qcow2")
	if !strings.Contains(frag, "file='/data/a&amp;b&apos;c.qcow2'") {
		t.Errorf("路径未转义:\n%s", frag)
	}
}

func TestNextFreeDevForBus(t *testing.T) {
	cases := []struct {
		bus  string
		want string
	}{
		{"virtio", "vdc"},
		{"scsi", "sdb"},
		{"sata", "sdb"},
		{"ide", "hdb"},
	}
	for _, c := range cases {
		if got := nextFreeDevForBus(testDomainXML, c.bus); got != c.want {
			t.Errorf("bus=%s got=%s want=%s", c.bus, got, c.want)
		}
	}
}

func TestSetTagAttrPreservesQuoteStyle(t *testing.T) {
	in := `<target dev='vda' bus='virtio'/>`
	out := setTagAttr(in, "bus", "scsi")
	if out != `<target dev='vda' bus='scsi'/>` {
		t.Errorf("get %q", out)
	}
	inserted := setTagAttr(`<target dev="vda"/>`, "bus", "scsi")
	if !strings.Contains(inserted, `bus="scsi"`) {
		t.Errorf("缺少属性时应插入且保留双引号: %q", inserted)
	}
}

func TestAddSCSIControllerIdempotent(t *testing.T) {
	if hasSCSIController(testDomainXML) {
		t.Fatal("测试域不应已有 SCSI 控制器")
	}
	out := addSCSIController(testDomainXML)
	if !hasSCSIController(out) {
		t.Fatalf("未插入控制器:\n%s", out)
	}
	if strings.Count(out, "controller type='scsi'") != 1 {
		t.Fatalf("控制器应只插入一次:\n%s", out)
	}
	if again := addSCSIController(out); again != out {
		t.Fatal("已有控制器时不应再插入")
	}
}

func TestElementText(t *testing.T) {
	if got := elementText(testDomainXML, "name"); got != "demo-vm" {
		t.Errorf("got %q", got)
	}
	if got := elementText(testDomainXML, "uuid"); got != "" {
		t.Errorf("不存在的元素应为空，got %q", got)
	}
}

func TestAddFDCControllerIdempotent(t *testing.T) {
	if hasFDCController(testDomainXML) {
		t.Fatal("测试域不应已有软驱控制器")
	}
	out := addFDCController(testDomainXML)
	if !hasFDCController(out) || strings.Count(out, "controller type='fdc'") != 1 {
		t.Fatalf("软驱控制器插入不对:\n%s", out)
	}
	if again := addFDCController(out); again != out {
		t.Fatal("已有控制器时不应再插入")
	}
}
