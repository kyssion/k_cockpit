package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// 缺陷 17 的端到端用例（virt+ide 被拒且不 detach、pc+ide 放行）在
// cdrom_bus_test.go 里已由另一位工程师覆盖，本文件只补他覆盖不到的两块：
//
//  1. 机型 → IDE 能力的**纯判定**，尤其是「定义里没有 machine 属性时按架构
//     推断」这条分支——它读 runtime.GOARCH，测试进程里改不了，只能通过把
//     arch 显式传入来覆盖；
//  2. 边界：virt 机型只拦 ide，sata/scsi 不能被误拦。

// TestIDESupportedByMachine 覆盖机型 → IDE 能力判定。
func TestIDESupportedByMachine(t *testing.T) {
	cases := []struct {
		machine string
		arch    string
		want    bool
		why     string
	}{
		{"", "amd64", true, "x86 默认机型 i440fx 有 IDE"},
		{"", "arm64", false, "arm64 默认机型 virt 没有 IDE"},
		{"virt", "aarch64", false, "virt 机型没有 IDE 控制器"},
		{"virt", "amd64", false, "virt 与架构无关，一律拒绝"},
		{"virt-8.2", "arm64", false, "virt-* 变体同样没有 IDE"},
		{"pc-i440fx-8.2", "amd64", true, "pc-* 有 IDE"},
		{"q35", "amd64", true, "q35 有 IDE"},
		{"Q35", "arm64", true, "机型名大小写不敏感"},
		{"  virt  ", "amd64", false, "首尾空白不影响判定"},
	}
	for _, c := range cases {
		if got := ideSupportedByMachine(c.machine, c.arch); got != c.want {
			t.Errorf("ideSupportedByMachine(%q, %q) = %v，应为 %v（%s）",
				c.machine, c.arch, got, c.want, c.why)
		}
	}
}

// machineTestVirsh 生成假 virsh：dumpxml 输出给定定义，detach/attach 追加
// 到调用记录。名字带前缀是为了不和 cdrom_bus_test.go 的助手撞名。
func machineTestVirsh(t *testing.T, domainXML string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "virsh.calls")
	xmlPath := filepath.Join(dir, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(domainXML), 0o644); err != nil {
		t.Fatalf("写入域定义失败: %v", err)
	}
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n" +
			"case \"$*\" in\n" +
			"  *dumpxml*) cat " + xmlPath + " ;;\n" +
			"  *detach-device*) printf 'detach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"  *attach-device*) printf 'attach %s\\n' \"$(cat \"$last\")\" >> " + logPath + " ;;\n" +
			"esac\n",
	})
	return logPath
}

func machineTestCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("读取调用记录失败: %v", err)
	}
	return string(b)
}

// TestCDROMBusSataOnVirtMachineStillAllowed 守住"只拦 ide"这个边界：
// sata/scsi 在任何机型上都有控制器，virt 机型不该被误拦——否则
// aarch64 节点上的光驱会彻底没法换总线。
func TestCDROMBusSataOnVirtMachineStillAllowed(t *testing.T) {
	const virtDomain = `<domain type='kvm'>
  <os><type arch='aarch64' machine='virt'>hvm</type></os>
  <devices>
    <disk type='file' device='cdrom'>
      <target dev='hda' bus='ide'/>
      <readonly/>
    </disk>
  </devices>
</domain>`
	logPath := machineTestVirsh(t, virtDomain)

	res, err := vmCDROMApply(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"action":   "bus",
			"from_bus": "ide",
			"bus":      "sata",
			"order_no": float64(0),
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmCDROMApply: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("virt 机型换到 sata 应当允许，实际: %+v", res)
	}
	calls := machineTestCalls(t, logPath)
	if !strings.Contains(calls, "detach ") || !strings.Contains(calls, "attach ") {
		t.Fatalf("virt 机型换到 sata 应当完成摘旧挂新，实际调用:\n%s", calls)
	}
}
