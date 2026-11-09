package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// cdromDomainXML 是一个带 ide 光驱（hda，序号 0）的域定义：换总线要摘的就是它。
const cdromDomainXML = `<domain type='kvm'>
  <devices>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/srv/old.iso'/>
      <target dev='hda' bus='ide'/>
      <readonly/>
    </disk>
  </devices>
</domain>`

// writeDomainXML 把域定义写到临时文件，供假 virsh 的 dumpxml 分支 cat。
//
// 走文件而不是把 XML 塞进 shell 字符串：单引号、空格与换行在
// `sh -c` 的引号里太容易出错，而这里要验证的是命令序列，不是引号转义。
func writeDomainXML(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "domain.xml")
	if err := os.WriteFile(p, []byte(cdromDomainXML), 0o644); err != nil {
		t.Fatalf("写入域定义失败: %v", err)
	}
	return p
}

// domainXMLWithMachine 在磁盘定义之上补一段 <os>，用来表达"机型"。
//
// 机型决定了 IDE 控制器存不存在：q35 / pc-* 有，virt（aarch64 常用）没有。
func domainXMLWithMachine(machine string) string {
	return `<domain type='kvm'>
  <os><type arch='aarch64' machine='` + machine + `'>hvm</type></os>
  <devices>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/srv/old.iso'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>
  </devices>
</domain>`
}

// cdromApply 跑一次 vmCDROMApply，返回结果与 virsh 的调用记录。
func cdromApply(t *testing.T, machine, bus string) (res *agent.Result, calls string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "virsh.calls")
	xmlPath := filepath.Join(dir, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(domainXMLWithMachine(machine)), 0o644); err != nil {
		t.Fatalf("写入域定义失败: %v", err)
	}
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n" +
			"echo \"$*\" >> " + logPath + "\n" +
			"case \"$*\" in\n" +
			"  *dumpxml*) cat " + xmlPath + " ;;\n" +
			"  *detach-device*) printf 'detach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"  *attach-device*) printf 'attach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"esac\n",
	})

	out, err := vmCDROMApply(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"action": "bus", "order_no": float64(0),
			"from_bus": "sata", "bus": bus,
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	b, _ := os.ReadFile(logPath)
	return out, string(b)
}

// TestCDROMBusRejectsIDEOnVirtMachine 覆盖「换总线成功、但机器变成开不了机」。
//
// 实机现象：aarch64 + machine=virt 的机器把光驱换成 ide，任务成功、
// `virsh dumpxml` 里能看到 `<target dev='hda' bus='ide'/>`，之后开机失败：
// `unsupported configuration: IDE controllers are unsupported for this QEMU
// binary or machine type`。
//
// 因此必须在**动手之前**拒掉，且不能先 detach（否则用户还会丢一个光驱）。
func TestCDROMBusRejectsIDEOnVirtMachine(t *testing.T) {
	res, calls := cdromApply(t, "virt", "ide")

	if res.Success {
		t.Fatal("virt 机型上换到 ide 必须被拒绝（否则机器开不了机）")
	}
	if !strings.Contains(res.Message, "不支持 IDE") {
		t.Errorf("失败原因应指向机型不支持 IDE: %q", res.Message)
	}
	if strings.Contains(calls, "detach") || strings.Contains(calls, "attach") {
		t.Errorf("被拒绝时不得改动域（否则光驱会被摘掉）：%q", calls)
	}
}

// TestCDROMBusAllowsIDEOnPCMachine 覆盖反面：pc/q35 机型有 IDE 控制器。
//
// 只测拒绝的用例拦不住一个"一律拒绝 ide"的实现，而那会让 x86 机器少一条
// 本来可用的总线。
func TestCDROMBusAllowsIDEOnPCMachine(t *testing.T) {
	res, calls := cdromApply(t, "pc-i440fx-8.2", "ide")

	if !res.Success {
		t.Fatalf("pc 机型上换到 ide 应当允许: %s", res.Message)
	}
	if !strings.Contains(calls, "attach") || !strings.Contains(calls, "dev='hda'") {
		t.Errorf("应把光驱挂成 hda（ide 设备名）：%q", calls)
	}
}

// TestSwitchCDROMBusDetachThenAttach 覆盖换总线的命令序列。
//
// libvirt 没有"改总线"这种增量操作：只能 detach 旧设备、attach 新设备。
// 旧设备名由**旧总线**决定（ide 是 hd*，sata/scsi 是 sd*），新设备名由新
// 总线决定——两者都算错就会出现"摘了一块不存在的设备"或"挂上一块同名设备"。
func TestSwitchCDROMBusDetachThenAttach(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "virsh.calls")
	xmlPath := writeDomainXML(t, dir)
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n" +
			"case \"$*\" in\n" +
			"  *dumpxml*) cat " + xmlPath + " ;;\n" +
			"  *detach-device*) printf 'detach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"  *attach-device*) printf 'attach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"esac\n",
	})

	iso := "/var/lib/k_cockpit/userdata/iso/ubuntu.iso"
	if err := switchCDROMBus(context.Background(), "demo-vm", 0,
		"ide", "sata", iso); err != nil {
		t.Fatalf("换总线失败: %v", err)
	}

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取调用记录失败: %v", err)
	}
	log := string(b)

	detachIdx := strings.Index(log, "detach ")
	attachIdx := strings.Index(log, "attach ")
	if detachIdx < 0 {
		t.Fatalf("没有摘除旧设备: %q", log)
	}
	if attachIdx < 0 {
		t.Fatalf("没有挂载新设备: %q", log)
	}
	// 顺序必须是先摘后挂：反过来会在同一条总线上短暂出现两块同名设备，
	// 而 libvirt 对重复 dev 的处理是直接拒绝。
	if detachIdx > attachIdx {
		t.Errorf("顺序反了，应当先 detach 再 attach: %q", log)
	}

	detached := log[detachIdx:attachIdx]
	// 旧设备名由**旧总线**算出来：ide + order 0 → hda，sata 会是 sda。
	if !strings.Contains(detached, "dev='hda'") || !strings.Contains(detached, "bus='ide'") {
		t.Errorf("摘除的应当是旧总线的设备 hda/ide，实际: %q", detached)
	}
	attached := log[attachIdx:]
	if !strings.Contains(attached, "dev='sda'") || !strings.Contains(attached, "bus='sata'") {
		t.Errorf("挂上的应当是目标总线的设备 sda/sata，实际: %q", attached)
	}
	// 介质要保留：用户换总线不是为了弹盘。
	if !strings.Contains(attached, "file='"+iso+"'") {
		t.Errorf("换总线后应保留原介质，实际: %q", attached)
	}
}

// TestSwitchCDROMBusReattachesOldDeviceOnFailure 覆盖失败时的兜底。
//
// 新设备挂不上时若不把旧设备挂回去，用户会拿到一台**光驱消失**的机器——
// 那比"换总线失败"糟得多，因为原来的盘也读不到了。
func TestSwitchCDROMBusReattachesOldDeviceOnFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "virsh.calls")
	xmlPath := writeDomainXML(t, dir)
	// 挂 sata 设备时失败（模拟总线不支持/参数被拒），挂 ide 的成功。
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n" +
			"case \"$*\" in\n" +
			"  *dumpxml*) cat " + xmlPath + " ;;\n" +
			"  *detach-device*) printf 'detach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"  *attach-device*)\n" +
			"    printf 'attach %s\\n' \"$(cat \"$last\")\" >> " + logPath + "\n" +
			"    case \"$(cat \"$last\")\" in *\"bus='sata'\"*) echo 'unsupported bus' >&2; exit 1 ;; esac\n" +
			"    ;;\n" +
			"esac\n",
	})

	err := switchCDROMBus(context.Background(), "demo-vm", 0,
		"ide", "sata", "/var/lib/k_cockpit/userdata/iso/ubuntu.iso")
	if err == nil {
		t.Fatal("新总线挂载失败时应当返回错误")
	}
	if !strings.Contains(err.Error(), "按新总线") {
		t.Errorf("错误应指向换总线这一步，实际: %v", err)
	}

	b, _ := os.ReadFile(logPath)
	log := string(b)
	if n := strings.Count(log, "attach "); n != 2 {
		t.Fatalf("应尝试挂新设备并回挂旧设备（共 2 次挂载），实际 %d 次: %q", n, log)
	}
	// 第二次挂载必须是旧设备的（ide/hdc），说明回挂真的发生了。
	if !strings.Contains(log[strings.LastIndex(log, "attach "):], "dev='hda'") {
		t.Errorf("最后一次挂载应是把旧设备挂回去: %q", log)
	}
}
