package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// withTrashDir 把回收站目录指到临时目录，测试结束后还原。
func withTrashDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := trashDir
	trashDir = dir
	t.Cleanup(func() { trashDir = old })
	return dir
}

// TestVMRestoreWithoutSavedXML 覆盖"记录还在、域定义没留存"的情形。
//
// 旧版本删除的记录没有域 XML，此时必须明确说"恢复不了"，而不是返回成功
// 或重建出一个空壳——用户据此可以选择彻底删除后用磁盘重建。
func TestVMRestoreWithoutSavedXML(t *testing.T) {
	withTrashDir(t)
	installFakeCommands(t, map[string]string{
		// 域不存在：dominfo 失败
		"virsh": "#!/bin/sh\nexit 1\n",
	})

	res, err := vmRestore(context.Background(), agent.Operation{Target: "gone-vm"}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmRestore 不应返回 error（业务失败用 Success=false）: %v", err)
	}
	if res.Success {
		t.Fatal("没有留存域配置时不得报告成功")
	}
	if !strings.Contains(res.Message, "回收站") {
		t.Errorf("拒绝文案应说明回收站里没有配置: %s", res.Message)
	}
}

// TestVMRestoreDefinesDomainFromSavedXML 覆盖正常恢复：
// 域不在 → 读留存 XML → virsh define → 复核域已存在。
func TestVMRestoreDefinesDomainFromSavedXML(t *testing.T) {
	dir := withTrashDir(t)
	marker := filepath.Join(dir, "defined")
	xml := `<domain type='kvm'><name>restored-vm</name></domain>`
	if err := os.WriteFile(filepath.Join(dir, "restored-vm.xml"), []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}

	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n" +
			"  *dominfo*) [ -f " + marker + " ] && exit 0 || exit 1 ;;\n" +
			"  *define*) touch " + marker + " ; exit 0 ;;\n" +
			"esac\n",
	})

	res, err := vmRestore(context.Background(), agent.Operation{Target: "restored-vm"}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmRestore: %v", err)
	}
	if !res.Success {
		t.Fatalf("恢复失败: %s", res.Message)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("应当调用 virsh define 重建域定义")
	}
}

// TestVMRestoreIsIdempotent 覆盖"域已存在"：直接成功，不重复 define。
func TestVMRestoreIsIdempotent(t *testing.T) {
	dir := withTrashDir(t)
	marker := filepath.Join(dir, "defined")
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n  *dominfo*) exit 0 ;;\n  *define*) touch " + marker + " ; exit 0 ;;\nesac\n",
	})
	res, err := vmRestore(context.Background(), agent.Operation{Target: "alive-vm"}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("域已存在时应直接成功: err=%v res=%+v", err, res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("域已存在时不应重复 define")
	}
}

// TestVMRestoreRejectsUnsafeName 覆盖名字参与路径拼接时的越界防护。
func TestVMRestoreRejectsUnsafeName(t *testing.T) {
	withTrashDir(t)
	installFakeCommands(t, map[string]string{"virsh": "#!/bin/sh\nexit 1\n"})
	for _, name := range []string{"", "../etc/passwd", "a/b", ".."} {
		res, _ := vmRestore(context.Background(), agent.Operation{Target: name}, func(Stage) {})
		if res.Success {
			t.Errorf("非法名字 %q 不应成功", name)
		}
	}
}
