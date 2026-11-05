package nodeagent

import (
	"strings"
	"testing"
)

const configXML = `<domain type='kvm'>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
  </os>
  <vcpu placement='static'>2</vcpu>
  <cpu mode='host-passthrough'/>
  <features>
    <acpi/>
    <apic/>
  </features>
  <clock offset='utc'/>
  <devices>
    <video><model type='virtio'/></video>
  </devices>
</domain>`

func TestSetBootOrderMapsDiskToHD(t *testing.T) {
	out, changed := setBootOrder(configXML, "disk,cdrom,network")
	if !changed {
		t.Fatal("引导顺序应产生变化")
	}
	for _, want := range []string{"<boot dev='hd'/>", "<boot dev='cdrom'/>", "<boot dev='network'/>"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q:\n%s", want, out)
		}
	}
	// 旧的 boot 必须先删掉，否则会出现两个 hd。
	if strings.Count(out, "<boot dev='hd'/>") != 1 {
		t.Errorf("重复的 hd 引导项:\n%s", out)
	}
}

func TestSetBootOrderUnknownTokenKeptOut(t *testing.T) {
	out, _ := setBootOrder(configXML, "bogus,disk")
	if strings.Contains(out, "bogus") {
		t.Errorf("未知设备名不应写入 XML:\n%s", out)
	}
}

func TestSetVideoModelAndNone(t *testing.T) {
	out, changed := setVideoModel(configXML, "qxl")
	if !changed || !strings.Contains(out, "<model type='qxl'/>") {
		t.Errorf("显示设备未替换:\n%s", out)
	}

	out, changed = setVideoModel(configXML, "none")
	if !changed || strings.Contains(out, "<video>") {
		t.Errorf("选「无」应移除 video 元素:\n%s", out)
	}
}

func TestSetClockOffset(t *testing.T) {
	out, changed := setClockOffset(configXML, "localtime")
	if !changed || !strings.Contains(out, "offset='localtime'") {
		t.Errorf("时钟口径未替换:\n%s", out)
	}
}

func TestSetFeatureFlag(t *testing.T) {
	out, changed := setFeatureFlag(configXML, "pae", true)
	if !changed || !strings.Contains(out, "<pae/>") {
		t.Errorf("PAE 未开启:\n%s", out)
	}

	out, changed = setFeatureFlag(configXML, "apic", false)
	if !changed || strings.Contains(out, "<apic/>") {
		t.Errorf("APIC 未关闭:\n%s", out)
	}
	// 其它特性必须保留。
	if !strings.Contains(out, "<acpi/>") {
		t.Errorf("关闭 APIC 时误删了 ACPI:\n%s", out)
	}
}

func TestSetKVMHidden(t *testing.T) {
	out, changed := setKVMHidden(configXML, true)
	if !changed || !strings.Contains(out, "<kvm><hidden state='on'/></kvm>") {
		t.Errorf("KVM 隐藏未写入:\n%s", out)
	}

	out, changed = setKVMHidden(out, false)
	if !changed || strings.Contains(out, "<kvm>") {
		t.Errorf("KVM 隐藏未移除:\n%s", out)
	}
}

func TestSetWatchdog(t *testing.T) {
	out, changed := setWatchdog(configXML, "poweroff")
	if !changed || !strings.Contains(out, "<watchdog model='i6300esb' action='poweroff'/>") {
		t.Errorf("看门狗未写入:\n%s", out)
	}

	out, changed = setWatchdog(out, "none")
	if !changed || strings.Contains(out, "<watchdog") {
		t.Errorf("看门狗未移除:\n%s", out)
	}
}

func TestSetCPUTopologyOnSelfClosingCPU(t *testing.T) {
	out, changed := setCPUTopology(configXML, 2, 2, 2)
	if !changed {
		t.Fatal("拓扑应产生变化")
	}
	if !strings.Contains(out, "<topology sockets='2' cores='2' threads='2'/>") {
		t.Errorf("拓扑未写入:\n%s", out)
	}
	// 自闭合的 <cpu/> 必须变成带子元素的形式，否则 XML 非法。
	if !strings.Contains(out, "</cpu>") {
		t.Errorf("cpu 元素未闭合:\n%s", out)
	}
}

func TestApplyCPUTuneQuotaAndAffinity(t *testing.T) {
	out, changed := applyCPUTune(configXML, "20000", "0-3", 2)
	if !changed {
		t.Fatal("cputune 应产生变化")
	}
	for _, want := range []string{
		"<cputune>", "<quota>20000</quota>",
		"<vcpupin vcpu='0' cpuset='0-3'/>", "<vcpupin vcpu='1' cpuset='0-3'/>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q:\n%s", want, out)
		}
	}

	// 清空配额与绑核时整块移除。
	out2, changed := applyCPUTune(out, "", "", 2)
	if !changed || strings.Contains(out2, "<cputune>") {
		t.Errorf("cputune 未移除:\n%s", out2)
	}
}

func TestSetMemoryHugepages(t *testing.T) {
	out, changed := setMemoryHugepages(configXML, true)
	if !changed || !strings.Contains(out, "<hugepages/>") {
		t.Errorf("大页未开启:\n%s", out)
	}
	out, changed = setMemoryHugepages(out, false)
	if !changed || strings.Contains(out, "<memoryBacking>") {
		t.Errorf("大页未关闭:\n%s", out)
	}
}

func TestApplyConfigXMLCombined(t *testing.T) {
	out, applied := applyConfigXML(configXML, map[string]any{
		"boot_order":        "disk,cdrom",
		"rtc_mode":          "localtime",
		"apic":              false,
		"cpu_sockets":       2,
		"cpu_cores":         2,
		"cpu_threads":       2,
		"cpu_limit_percent": 50,
		"cpu_affinity":      "0-3",
	})
	for _, key := range []string{
		"boot_order", "rtc_mode", "apic", "cpu_sockets/cpu_cores/cpu_threads",
		"cpu_limit_percent/cpu_affinity",
	} {
		if !contains(applied, key) {
			t.Errorf("未记录已应用键 %q: %v", key, applied)
		}
	}
	// 50% × 2 vCPU × 100 = 10000
	if !strings.Contains(out, "<quota>10000</quota>") {
		t.Errorf("配额换算不对:\n%s", out)
	}
	if strings.Count(out, "<vcpupin") != 2 {
		t.Errorf("vcpupin 数量应对齐 vCPU 数:\n%s", out)
	}
}

func TestApplyConfigXMLNoop(t *testing.T) {
	out, applied := applyConfigXML(configXML, map[string]any{"rtc_mode": "utc"})
	if len(applied) != 0 {
		t.Errorf("等值配置不应产生变更: %v", applied)
	}
	if out != configXML {
		t.Error("等值配置不应修改 XML")
	}
}
