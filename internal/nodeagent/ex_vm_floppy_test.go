package nodeagent

import (
	"strings"
	"testing"
)

func TestFloppyFragmentAndBlock(t *testing.T) {
	xml := `<domain><devices>
	  <disk type='file' device='floppy'>
	    <source file='/var/lib/k_cockpit/userdata/1/floppy.img'/>
	    <target dev='fda' bus='fdc'/>
	  </disk>
	</devices></domain>`
	block, ok := floppyBlockOf(xml)
	if !ok || !strings.Contains(block, "device='floppy'") {
		t.Fatalf("未找到软盘块: %q", block)
	}
	frag := floppyFragment("/tmp/a.img")
	if !strings.Contains(frag, "dev='fda'") || !strings.Contains(frag, "bus='fdc'") {
		t.Errorf("软盘片段不对:\n%s", frag)
	}
}

func TestNVRAMPathAndTemplate(t *testing.T) {
	xml := `<domain><os><type arch='x86_64'>hvm</type>
	  <nvram template='/usr/share/OVMF/OVMF_VARS.fd'>/var/lib/libvirt/qemu/nvram/demo_VARS.fd</nvram>
	</os></domain>`
	target, template, ok := nvramPathAndTemplate(xml)
	if !ok || target != "/var/lib/libvirt/qemu/nvram/demo_VARS.fd" ||
		template != "/usr/share/OVMF/OVMF_VARS.fd" {
		t.Fatalf("解析结果 target=%q template=%q ok=%v", target, template, ok)
	}
	if _, _, ok := nvramPathAndTemplate("<domain></domain>"); ok {
		t.Fatal("非 UEFI 域不应命中")
	}
}

func TestBootFromCDROMFirst(t *testing.T) {
	xml := `<domain><os type='hvm'><type arch='x86_64'>hvm</type></os></domain>`
	out := bootFromCDROMFirst(xml)
	if !strings.Contains(out, "<boot dev='cdrom'/>") {
		t.Fatalf("未插入 CDROM 启动项:\n%s", out)
	}
	if strings.Index(out, "boot dev='cdrom'") > strings.Index(out, "</os>") {
		t.Fatal("启动项应插在 <os> 内")
	}
}
