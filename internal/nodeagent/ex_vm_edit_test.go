package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// installFakeCommands 把测试用的 virsh / qemu-img / virt-xml-validate 放到
// PATH 最前面。执行器只通过命令边界与系统交互，因此这层替身足以覆盖
// 「参数如何变成命令、结果如何变成 Result」这段逻辑。
func installFakeCommands(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("写入假命令 %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestVMAdoptReturnsUUID(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n  *domuuid*) echo '11111111-2222-3333-4444-555555555555' ;;\n  *) exit 0 ;;\nesac\n",
	})

	res, err := vmAdopt(context.Background(), agent.Operation{Target: "demo-vm"}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmAdopt: %v", err)
	}
	if !res.Success {
		t.Fatalf("失败: %s", res.Message)
	}
	info, ok := res.Data[agent.VMAdoptDataKey].(agent.VMAdoptInfo)
	if !ok || info.UUID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("UUID 结果不对: %#v", res.Data)
	}
}

func TestVMXMLApplyRejectsNameMismatch(t *testing.T) {
	res, err := vmXMLApply(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"action": "validate",
			"xml":    "<domain><name>another-vm</name></domain>",
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("名称不一致应失败，避免覆盖另一台域")
	}
}

func TestVMXMLApplyValidateOK(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virt-xml-validate": "#!/bin/sh\nexit 0\n",
	})
	res, err := vmXMLApply(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"action": "validate",
			"xml":    "<domain><name>demo-vm</name><devices/></domain>",
		},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("validate 应成功: err=%v res=%+v", err, res)
	}
	info, ok := res.Data[agent.VMXMLValidateKey].(agent.VMXMLValidateInfo)
	if !ok || !info.Valid {
		t.Fatalf("校验结果不对: %#v", res.Data)
	}
}

func TestVMDiskResizeRejectsRunning(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n  *domstate*) echo 'running' ;;\n  *) exit 0 ;;\nesac\n",
	})
	res, err := vmDiskResize(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"old_gb": float64(20), "new_gb": float64(40)},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "运行") {
		t.Fatalf("运行中应拒绝，得到: %+v", res)
	}
}

func TestVMDiskResizeExpandsImage(t *testing.T) {
	xmlPath := filepath.Join(t.TempDir(), "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(testDomainXML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KC_TEST_XML", xmlPath)
	installFakeCommands(t, map[string]string{
		"virsh":    "#!/bin/sh\ncase \"$*\" in\n  *domstate*) echo 'shut off' ;;\n  *dumpxml*) cat \"$KC_TEST_XML\" ;;\n  *) exit 0 ;;\nesac\n",
		"qemu-img": "#!/bin/sh\ncase \"$1\" in\n  info) echo '{\"virtual-size\": 42949672960}' ;;\n  *) exit 0 ;;\nesac\n",
	})

	res, err := vmDiskResize(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"old_gb": float64(20), "new_gb": float64(40)},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("扩容应成功: err=%v res=%+v", err, res)
	}
	info, ok := res.Data[agent.VMDiskDataKey].(agent.VMDiskResizeInfo)
	if !ok || !info.Applied || info.NewGB != 40 || !info.GuestGrowNeeded {
		t.Fatalf("扩容结果不对: %#v", res.Data)
	}
}

func TestVMDisksIndependentRejectsRunning(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n  *domstate*) echo 'running' ;;\n  *) exit 0 ;;\nesac\n",
	})
	res, err := vmDisksIndependent(context.Background(), agent.Operation{
		Target: "demo-vm",
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "关机") {
		t.Fatalf("运行中应拒绝，得到: %+v", res)
	}
}

func TestDiskAttachRejectsPathTraversal(t *testing.T) {
	res, err := diskAttach(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"rel_path": "../etc/passwd"},
	}, func(Stage) {}, false)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "越出") {
		t.Fatalf("越界路径应拒绝，得到: %+v", res)
	}
}
