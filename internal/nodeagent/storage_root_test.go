package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k_cockpit/internal/agent"
)

// withAllowedRoot 把白名单与状态目录都指到临时目录，测试后还原。
func withAllowedRoot(t *testing.T) (root string) {
	t.Helper()
	dir := t.TempDir()
	oldRoots, oldDefault := userdataAllowedRoots, userdataRoot()
	oldState := agentStateDir
	userdataAllowedRoots = []string{dir}
	agentStateDir = dir
	t.Cleanup(func() {
		userdataAllowedRoots = oldRoots
		userdataRootValue.Store(oldDefault)
		agentStateDir = oldState
	})
	return filepath.Join(dir, "pool", "userdata")
}

// TestStorageRootSwitchesAndPersists 覆盖根目录切换与落盘。
//
// 实机抓出：根目录写死在系统盘，节点建了数据池也用不上，600MB 镜像直接
// 写进根分区。切换后必须生效并落盘，否则 agent 一重启又回到系统盘。
func TestStorageRootSwitchesAndPersists(t *testing.T) {
	target := withAllowedRoot(t)

	res, err := storageRootSet(context.Background(), agent.Operation{
		Params: map[string]any{"path": target},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("切换失败: err=%v res=%+v", err, res)
	}
	if got := userdataRoot(); got != target {
		t.Fatalf("生效的根 = %q, 期望 %q", got, target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("目标目录应被创建: %v", err)
	}
	b, err := os.ReadFile(storageRootFile())
	if err != nil {
		t.Fatalf("配置未落盘: %v", err)
	}
	if string(b) != target+"\n" {
		t.Errorf("落盘内容 = %q", string(b))
	}

	// 空路径表示没有默认池：回到历史默认根，并清掉落盘配置。
	res, err = storageRootSet(context.Background(), agent.Operation{
		Params: map[string]any{"path": ""},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("复位失败: err=%v res=%+v", err, res)
	}
	if got := userdataRoot(); got != defaultUserdataRoot {
		t.Errorf("复位后的根 = %q, 期望 %q", got, defaultUserdataRoot)
	}
	if _, err := os.Stat(storageRootFile()); !os.IsNotExist(err) {
		t.Error("复位后应删除落盘配置")
	}
}

// TestStorageRootRejectsOutside 覆盖"控制面被攻破也不能乱写"这条底线。
func TestStorageRootRejectsOutside(t *testing.T) {
	withAllowedRoot(t)
	for _, p := range []string{"/etc", "/var/lib/k_cockpit/../etc", "relative/path"} {
		res, err := storageRootSet(context.Background(), agent.Operation{
			Params: map[string]any{"path": p},
		}, func(Stage) {})
		if err != nil {
			t.Fatalf("%q 不应返回 error: %v", p, err)
		}
		if res.Success {
			t.Errorf("%q 不在白名单内，不应成功", p)
		}
	}
}

// TestLoadUserdataRootAppliesSavedValue 覆盖重启后读回配置。
func TestLoadUserdataRootAppliesSavedValue(t *testing.T) {
	target := withAllowedRoot(t)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(storageRootFile(), []byte(target+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	LoadUserdataRoot()
	if got := userdataRoot(); got != target {
		t.Fatalf("启动加载后根 = %q, 期望 %q", got, target)
	}
}
