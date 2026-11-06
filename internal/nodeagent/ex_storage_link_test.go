package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k_cockpit/internal/agent"
)

// 本文件覆盖第 3 轮 DEF-R3-05 的节点侧：秒传必须把内容真正落到目标路径。
//
// 真机证据：`instant=true`、返回新 `rel_path`，但该路径 `os.path.isfile=false`；
// 后续按 rel_path 取文件全部失败。

// TestLinkOrCopyFileHardlinks 覆盖同文件系统下的首选路径：硬链接。
func TestLinkOrCopyFileHardlinks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.iso")
	if err := os.WriteFile(src, []byte("content-"+"x"), 0o644); err != nil {
		t.Fatalf("写源文件失败: %v", err)
	}
	dst := filepath.Join(dir, "dst.iso")

	if err := linkOrCopyFile(src, dst); err != nil {
		t.Fatalf("落地失败: %v", err)
	}
	// 目标必须真的存在（这正是缺陷的判别点）。
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("目标文件不存在（DEF-R3-05 复现）: %v", err)
	}
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if !os.SameFile(si, di) {
		t.Error("同文件系统下应建立硬链接（不搬字节）")
	}
	if di.Size() != si.Size() {
		t.Errorf("大小不一致: src=%d dst=%d", si.Size(), di.Size())
	}
}

// TestLinkOrCopyFileRefusesExistingTarget 覆盖"不悄悄覆盖同名文件"。
//
// DB 的唯一约束会拒绝这条记录；如果先覆盖了文件，用户已有内容就被替换了，
// 而记录还是旧的——比直接失败糟得多。
func TestLinkOrCopyFileRefusesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.iso")
	dst := filepath.Join(dir, "dst.iso")
	_ = os.WriteFile(src, []byte("new"), 0o644)
	_ = os.WriteFile(dst, []byte("existing"), 0o644)

	if err := linkOrCopyFile(src, dst); err == nil {
		t.Fatal("目标已存在时应失败")
	}
	body, _ := os.ReadFile(dst)
	if string(body) != "existing" {
		t.Errorf("已存在的文件被覆盖了: %q", body)
	}
}

// TestLinkOrCopyFileMissingSource 覆盖来源不存在（记录与实际不符）。
func TestLinkOrCopyFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := linkOrCopyFile(filepath.Join(dir, "nope.iso"), filepath.Join(dir, "dst.iso")); err == nil {
		t.Fatal("来源不存在时应失败")
	}
}

// TestCopyFileSyncFallback 覆盖硬链接不可用时的回退路径（跨设备/不支持硬链接）。
//
// 直接测回退函数：跨设备在单机测试里造不出来，而这正是必须存在的分支
// （bind mount、独立分区、overlay、网络文件系统都不支持硬链接）。
func TestCopyFileSyncFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.iso")
	payload := []byte("fallback-copy-payload")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("写源文件失败: %v", err)
	}
	dst := filepath.Join(dir, "dst.iso")

	if err := copyFileSync(src, dst); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读目标失败（回退路径没落盘）: %v", err)
	}
	if string(body) != string(payload) {
		t.Errorf("内容不一致: %q", body)
	}
	si, _ := os.Stat(src)
	di, _ := os.Stat(dst)
	if os.SameFile(si, di) {
		t.Error("复制路径不该产生硬链接（应是一份独立副本）")
	}
	// 再复制一次到同一目标必须失败（O_EXCL：不覆盖）。
	if err := copyFileSync(src, dst); err == nil {
		t.Error("目标已存在时应失败（不覆盖）")
	}
}

// TestStorageFileCommitLinksFromSource 覆盖 commit 的 link_from_rel 分支。
//
// 用临时目录替换 userdataRoot 不可行（它是常量），因此这里只验证**越界拒绝**
// 这条与安全相关的分支：来源或目标带 ../ 必须被挡住，不能落盘到存储根之外。
func TestStorageFileCommitRejectsEscape(t *testing.T) {
	for _, params := range []map[string]any{
		{"rel_path": "../../etc/passwd", "link_from_rel": "share/a.iso"},
		{"rel_path": "share/dst.iso", "link_from_rel": "../../etc/passwd"},
	} {
		res, err := storageFileCommit(context.Background(), agent.Operation{Params: params}, func(Stage) {})
		if err != nil {
			t.Fatalf("不应返回 error: %v", err)
		}
		if res.Success {
			t.Errorf("越界路径必须被拒绝: %v", params)
		}
	}
}
