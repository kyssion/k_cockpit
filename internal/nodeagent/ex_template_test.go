package nodeagent

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

func TestTemplateDiskPathRejectsTraversal(t *testing.T) {
	for _, name := range []string{"", "..", "../etc/passwd", "a/b", `a\b`, "a..b"} {
		if _, err := templateDiskPath(name); err == nil {
			t.Errorf("模板名 %q 应被拒绝", name)
		}
	}
	p, err := templateDiskPath("ubuntu-24.04")
	if err != nil {
		t.Fatalf("正常名称被拒绝: %v", err)
	}
	if !strings.HasPrefix(p, templatesDir+"/") || !strings.HasSuffix(p, ".qcow2") {
		t.Errorf("路径不在模板目录: %s", p)
	}
}

func TestTemplateDiskPathsPrefersList(t *testing.T) {
	got := templateDiskPaths(map[string]any{
		"disk_path":  "/old.qcow2",
		"disk_paths": []any{"/a.qcow2", "/b.qcow2"},
	})
	if len(got) != 2 || got[0] != "/a.qcow2" || got[1] != "/b.qcow2" {
		t.Errorf("应优先使用 disk_paths: %#v", got)
	}

	fallback := templateDiskPaths(map[string]any{"disk_path": "/only.qcow2"})
	if len(fallback) != 1 || fallback[0] != "/only.qcow2" {
		t.Errorf("缺少 disk_paths 时应回退 disk_path: %#v", fallback)
	}
}

func TestTemplateDeleteRejectsOutsidePath(t *testing.T) {
	res, err := vmTemplateDelete(context.Background(), agent.Operation{
		Params: map[string]any{"disk_paths": []any{"/etc/passwd"}},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "模板目录之外") {
		t.Fatalf("目录外的路径应拒绝，得到: %+v", res)
	}
}

func TestTemplateDeleteMissingFileIsIdempotent(t *testing.T) {
	res, err := vmTemplateDelete(context.Background(), agent.Operation{
		Params: map[string]any{"disk_paths": []any{templatesDir + "/not-exist.qcow2"}},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("删除不存在的模板盘应幂等成功: err=%v res=%+v", err, res)
	}
}

func TestTemplatePrepareRejectsRunning(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n  *domstate*) echo 'running' ;;\n  *) exit 0 ;;\nesac\n",
	})
	res, err := vmTemplatePrepare(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"name": "tpl-1"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "关机") {
		t.Fatalf("运行中应拒绝，得到: %+v", res)
	}
}
