package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// fakeVirshForTune 让 virsh 把每次调用记进日志，并对 dumpxml / domstate
// 返回固定的最小输出。执行器与系统的边界只有命令，替身足以覆盖
// 「字段如何变成参数」这段逻辑。
func fakeVirshForTune(t *testing.T, state string) (logPath string) {
	t.Helper()
	logPath = filepath.Join(t.TempDir(), "virsh.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *dumpxml*) printf '%s\\n' \"<domain><devices><disk type='file' device='disk'><driver type='qcow2'/><target dev='vda' bus='virtio'/></disk></devices></domain>\" ;;\n" +
		"  *domstate*) echo '" + state + "' ;;\n" +
		"esac\n" +
		"exit 0\n"
	installFakeCommands(t, map[string]string{"virsh": script})
	return logPath
}

func readCalls(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取命令日志失败: %v", err)
	}
	return string(raw)
}

// TestVMConfigUpdateAppliesAllDiskLimits 覆盖 F-2-05 / F-2-06 的磁盘限速矩阵。
//
// 这些键此前只有 disk_iops_total 真正下发，其余五项会写库并返回成功——
// 用户看到"修改成功"，磁盘上却没有任何变化。
func TestVMConfigUpdateAppliesAllDiskLimits(t *testing.T) {
	logPath := fakeVirshForTune(t, "running")

	res, err := vmConfigUpdate(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"changes": map[string]any{
			"disk_iops_read":   float64(500),
			"disk_bytes_write": float64(20),
		}},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("限速下发失败: %s", res.Message)
	}

	calls := readCalls(t, logPath)
	// 运行中的域必须带 --live：矩阵把这几项标为「即时生效」，
	// 只写 --config 会让改动等到下次重启才生效。
	for _, want := range []string{
		"blkdeviotune demo-vm vda",
		"--read-iops-sec 500",
		"--write-bytes-sec 20971520", // 20 MB/s
		"--live",
		"--config",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("命令缺少 %q，实际调用:\n%s", want, calls)
		}
	}
	// 未提交的项不能补 0，否则会悄悄清掉用户上次设的限制。
	for _, unwanted := range []string{"--total-iops-sec", "--read-bytes-sec"} {
		if strings.Contains(calls, unwanted) {
			t.Errorf("未提交的项被下发 %q，实际调用:\n%s", unwanted, calls)
		}
	}
	if !strings.Contains(res.Message, "disk_iops_read") {
		t.Errorf("结果消息未列出已应用的键: %q", res.Message)
	}
}

// TestVMConfigUpdateDiskLimitsOffline 覆盖关机态：不应带 --live
// （libvirt 对未运行的域会拒绝该参数，导致整次修改失败）。
func TestVMConfigUpdateDiskLimitsOffline(t *testing.T) {
	logPath := fakeVirshForTune(t, "shut off")

	res, err := vmConfigUpdate(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"changes": map[string]any{
			"disk_iops_total": float64(1000),
		}},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("限速下发失败: %s", res.Message)
	}

	calls := readCalls(t, logPath)
	if !strings.Contains(calls, "--total-iops-sec 1000") {
		t.Errorf("缺少 total IOPS 参数:\n%s", calls)
	}
	if strings.Contains(calls, "--live") {
		t.Errorf("关机态不应带 --live:\n%s", calls)
	}
}

// TestDiskTuneArgsIgnoresAbsentKeys 直接锁住"只翻译出现的键"这条规则。
func TestDiskTuneArgsIgnoresAbsentKeys(t *testing.T) {
	args := diskTuneArgs(map[string]any{"disk_iops_read": float64(0)})
	joined := strings.Join(args, " ")
	if joined != "--read-iops-sec 0" {
		t.Fatalf("参数翻译不对: %q", joined)
	}

	if got := diskTuneArgs(map[string]any{"vcpu": float64(4)}); len(got) != 0 {
		t.Fatalf("非限速字段不应产生参数: %v", got)
	}
}
