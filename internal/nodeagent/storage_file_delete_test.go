package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k_cockpit/internal/agent"
)

// withUserdataRoots 把「当前根」与「历史根」都指到临时目录，并在用例结束
// 后复位。两个根都是包级变量（见 defaultUserdataRoot 的说明），因此这里
// 必须成对设置——否则「历史根回退」那条路径根本没被覆盖到。
func withUserdataRoots(t *testing.T) (current, legacy string) {
	t.Helper()
	dir := t.TempDir()
	current = filepath.Join(dir, "current")
	legacy = filepath.Join(dir, "legacy")
	for _, p := range []string{current, legacy} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
	}
	prevRoot, prevLegacy := userdataRoot(), defaultUserdataRoot
	userdataRootValue.Store(current)
	defaultUserdataRoot = legacy
	t.Cleanup(func() {
		userdataRootValue.Store(prevRoot)
		defaultUserdataRoot = prevLegacy
	})
	return current, legacy
}

// writeUserdataFile 在指定根下写一份文件，返回绝对路径。
func writeUserdataFile(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	return p
}

// TestStorageFileDeleteRemovesCurrentRootFile 覆盖最常见的一条路径。
func TestStorageFileDeleteRemovesCurrentRootFile(t *testing.T) {
	current, _ := withUserdataRoots(t)
	p := writeUserdataFile(t, current, "iso/ubuntu.iso")

	res, err := storageFileDelete(context.Background(), agent.Operation{
		Target: "iso/ubuntu.iso",
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("删除应成功: %s", res.Message)
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Errorf("文件仍存在: %v", statErr)
	}
	info, _ := res.Data[agent.StorageFileDeleteKey].(agent.StorageFileDeleteInfo)
	if !info.Deleted {
		t.Errorf("结果应标明确实删掉了一份文件: %+v", info)
	}
}

// TestStorageFileDeleteFallsBackToLegacyRoot 覆盖历史存储根。
//
// 实机抓出的孤儿文件全部落在 `/var/lib/k_cockpit/userdata`（存储根切到
// 存储池之前的默认位置）。只按当前根拼路径的话，这些文件永远删不掉——
// 而它们正是最该被清掉的那批。
func TestStorageFileDeleteFallsBackToLegacyRoot(t *testing.T) {
	current, legacy := withUserdataRoots(t)
	p := writeUserdataFile(t, legacy, "packages/old.tar.gz")

	res, err := storageFileDelete(context.Background(), agent.Operation{
		Target: "packages/old.tar.gz",
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("删除历史根文件应成功: err=%v res=%+v", err, res)
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Errorf("历史根里的文件未被删除: %v", statErr)
	}
	// 当前根里不该被凭空创建出同名文件。
	if _, statErr := os.Stat(filepath.Join(current, "packages/old.tar.gz")); !os.IsNotExist(statErr) {
		t.Errorf("删除不该在当前根创建文件: %v", statErr)
	}
}

// TestStorageFileDeleteMissingIsIdempotent 覆盖「本来就不在」。
//
// 返回失败会让控制面把一条记录永远留着（或者把一个用户无法解决的错误
// 摆在他面前），而删除的目标状态本来就是"这份内容不在"。
func TestStorageFileDeleteMissingIsIdempotent(t *testing.T) {
	withUserdataRoots(t)

	res, err := storageFileDelete(context.Background(), agent.Operation{
		Target: "iso/never-uploaded.iso",
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("文件不存在时应幂等成功: %s", res.Message)
	}
	info, _ := res.Data[agent.StorageFileDeleteKey].(agent.StorageFileDeleteInfo)
	if info.Deleted {
		t.Errorf("文件本来就不在，Deleted 应为 false: %+v", info)
	}
}

// TestStorageFileDeleteRejectsEscapingPath 覆盖越界。
//
// 请求里的路径一律是"相对存储根"。放行绝对路径或 `../` 等于把"能删宿主机
// 上的哪个文件"交给调用方——与共享目录（vm.share.mount）同一条安全边界。
func TestStorageFileDeleteRejectsEscapingPath(t *testing.T) {
	withUserdataRoots(t)

	for _, bad := range []string{
		"/etc/passwd",
		"../../etc/passwd",
		"../current-evil/x",
		"",
	} {
		res, err := storageFileDelete(context.Background(), agent.Operation{Target: bad}, func(Stage) {})
		if err != nil {
			t.Fatalf("路径 %q 不应返回 error: %v", bad, err)
		}
		if res.Success {
			t.Errorf("越界/空路径 %q 应被拒绝", bad)
		}
	}
}

// TestStorageFileDeleteRegistered 确认新能力已登记到节点处理器表：少了这一
// 条，控制面下发时会得到"不支持的任务类型"式的失败，而文件永远删不掉。
func TestStorageFileDeleteRegistered(t *testing.T) {
	if _, ok := NewRegistry()[agent.OpStorageFileDelete]; !ok {
		t.Fatal("storage.file.delete 未在节点处理器表中登记")
	}
}
