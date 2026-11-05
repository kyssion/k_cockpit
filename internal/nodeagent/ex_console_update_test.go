package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

const consoleBaseXML = `<domain type='kvm'>
  <devices>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>
    <video><model type='virtio'/></video>
  </devices>
</domain>`

// --- 纯函数：XML 定点编辑 ---

func TestApplyConsoleGraphicsExposesAndSetsPassword(t *testing.T) {
	out, changed := applyConsoleGraphics(consoleBaseXML, "vnc", true, "0.0.0.0", 5901, "s3cret")
	if !changed {
		t.Fatal("配置应产生变化")
	}
	for _, want := range []string{
		"listen='0.0.0.0'",
		"port='5901'",
		"autoport='no'",
		"passwd='s3cret'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q:\n%s", want, out)
		}
	}
	// 不能把显卡模型或其它设备写丢。
	if !strings.Contains(out, "<model type='virtio'/>") {
		t.Errorf("其它设备被破坏:\n%s", out)
	}
}

func TestApplyConsoleGraphicsIdempotent(t *testing.T) {
	// 目标状态与现状一致时不应产生变更，避免每次保存都触发一次 define。
	_, changed := applyConsoleGraphics(consoleBaseXML, "vnc", true, "127.0.0.1", 0, "")
	if changed {
		t.Error("等值配置不应标记为变更")
	}
}

func TestApplyConsoleGraphicsDisableRemovesElement(t *testing.T) {
	out, changed := applyConsoleGraphics(consoleBaseXML, "vnc", false, "127.0.0.1", 0, "")
	if !changed {
		t.Fatal("关闭控制台应产生变化")
	}
	if strings.Contains(out, "<graphics") {
		t.Errorf("graphics 元素未被删除:\n%s", out)
	}
	// 删除时不应留下空行。
	if strings.Contains(out, "\n\n") {
		t.Errorf("删除后留下了多余空行:\n%s", out)
	}
}

func TestApplyConsoleGraphicsInsertsMissingElement(t *testing.T) {
	xmlText := "<domain><devices><video><model type='virtio'/></video></devices></domain>"
	out, changed := applyConsoleGraphics(xmlText, "spice", true, "127.0.0.1", 0, "")
	if !changed {
		t.Fatal("缺少元素时应补一个")
	}
	if !strings.Contains(out, "<graphics type='spice' port='-1' autoport='yes' listen='127.0.0.1'/>") {
		t.Errorf("新增元素形状不对:\n%s", out)
	}
	if strings.Index(out, "<graphics") > strings.Index(out, "</devices>") {
		t.Error("graphics 必须插在 devices 内")
	}
}

func TestApplyConsoleGraphicsEscapesPassword(t *testing.T) {
	out, _ := applyConsoleGraphics(consoleBaseXML, "vnc", true, "127.0.0.1", 0, "a'b&c")
	if !strings.Contains(out, "passwd='a&apos;b&amp;c'") {
		t.Errorf("密码未按属性转义:\n%s", out)
	}
}

// --- 执行器：命令边界 ---

func TestVMConsoleUpdateWritesAndReportsRestart(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "virsh.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *dumpxml*) printf '%s\\n' \"" + consoleBaseXML + "\" ;;\n" +
		"  *domstate*) echo 'running' ;;\n" +
		"  *define*)\n" +
		"    last=''; for a in \"$@\"; do last=\"$a\"; done\n" +
		"    cat \"$last\" >> " + logPath + "\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	installFakeCommands(t, map[string]string{"virsh": script})

	res, err := vmConsoleUpdate(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"protocol": "vnc", "enabled": true, "listen": "0.0.0.0",
			"port": float64(5901), "password": "s3cret",
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("下发失败: %s", res.Message)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取命令日志失败: %v", err)
	}
	log := string(raw)
	if !strings.Contains(log, "define") {
		t.Errorf("没有调用 define:\n%s", log)
	}
	for _, want := range []string{
		"listen='0.0.0.0'", "port='5901'", "passwd='s3cret'",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("写入的域定义缺少 %q:\n%s", want, log)
		}
	}
	if !strings.Contains(res.Message, "重启") {
		t.Errorf("运行中的域应提示需重启: %q", res.Message)
	}
	if v, ok := res.Data[agent.ConsoleUpdateRestartRequiredKey].(bool); !ok || !v {
		t.Errorf("restart_required 应为 true: %#v", res.Data)
	}
}

func TestVMConsoleUpdateNoopSkipsDefine(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "virsh.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *dumpxml*) printf '%s\\n' \"" + consoleBaseXML + "\" ;;\n" +
		"  *domstate*) echo 'shut off' ;;\n" +
		"esac\n" +
		"exit 0\n"
	installFakeCommands(t, map[string]string{"virsh": script})

	res, err := vmConsoleUpdate(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"protocol": "vnc", "enabled": true, "listen": "127.0.0.1"},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("等值配置应成功: %v %s", err, res.Message)
	}

	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), " define ") {
		t.Errorf("等值配置不应调用 define:\n%s", raw)
	}
}
