package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrowDiskToResizesOnlyWhenNeeded 覆盖从模板创建时的系统盘扩容。
//
// 实机抓出：克隆路径直接复制模板盘，请求里的 disk_gb 被静默忽略——控制面
// 记录 10G、qemu-img 只有 3.5G，配额也按 10G 计费。这里锁住两条边界：
// 需要扩时必须扩，已经够大时不得动它（缩容会截断共享父盘的数据）。
func TestGrowDiskToResizesOnlyWhenNeeded(t *testing.T) {
	t.Run("容量不足时扩容", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "calls")
		installFakeCommands(t, map[string]string{
			"qemu-img": "#!/bin/sh\ncase \"$1\" in\n" +
				"  info) echo '{\"virtual-size\": 1073741824}' ;;\n" +
				"  resize) echo \"$*\" >> " + marker + " ;;\n" +
				"esac\n",
		})
		img := filepath.Join(dir, "disk.qcow2")
		if err := os.WriteFile(img, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := growDiskTo(context.Background(), img, 5); err != nil {
			t.Fatalf("扩容失败: %v", err)
		}
		b, _ := os.ReadFile(marker)
		if !strings.Contains(string(b), "resize") || !strings.Contains(string(b), "5G") {
			t.Errorf("应调用 qemu-img resize 5G，实际: %q", string(b))
		}
	})

	t.Run("容量已足够时不动作", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "calls")
		installFakeCommands(t, map[string]string{
			"qemu-img": "#!/bin/sh\ncase \"$1\" in\n" +
				"  info) echo '{\"virtual-size\": 21474836480}' ;;\n" +
				"  resize) echo \"$*\" >> " + marker + " ;;\n" +
				"esac\n",
		})
		img := filepath.Join(dir, "disk.qcow2")
		if err := os.WriteFile(img, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := growDiskTo(context.Background(), img, 5); err != nil {
			t.Fatalf("不应失败: %v", err)
		}
		if b, _ := os.ReadFile(marker); len(b) != 0 {
			t.Errorf("容量已足够时不得缩容或重复扩容，实际调用: %q", string(b))
		}
	})
}
