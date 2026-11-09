package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// snapshotFake 铺一套假的 virsh：snapshot-delete 按 deleteFails 决定成败，
// snapshot-list 输出 listOut 里的名字，所有调用记进 calls。
func snapshotFake(t *testing.T, deleteFails bool, listOut string) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "virsh.calls")
	listFile := filepath.Join(dir, "snapshots.txt")
	if err := os.WriteFile(listFile, []byte(listOut), 0o644); err != nil {
		t.Fatalf("写入快照清单失败: %v", err)
	}
	fail := "exit 0"
	if deleteFails {
		// 尽量贴近 virsh 的真实文案：名字在最后一段。
		fail = `echo "error: Domain snapshot $last not found" >&2; exit 1`
	}
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n" +
			"echo \"$*\" >> " + calls + "\n" +
			"case \"$*\" in\n" +
			"  *snapshot-list*) cat " + listFile + " ;;\n" +
			"  *snapshot-delete*) " + fail + " ;;\n" +
			"esac\n",
	})
	return calls
}

// TestSnapshotDeleteRequiresSnapshotName 覆盖「空名字直接执行 virsh」。
//
// 实机堆栈：`Failed to get option 'snapshotname': Option argument is empty`
// ——那句话与"快照是否存在"毫无关系，看日志的人根本猜不到是参数没传。
func TestSnapshotDeleteRequiresSnapshotName(t *testing.T) {
	calls := snapshotFake(t, false, "")

	res, err := vmSnapshotDelete(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("缺少快照名时必须失败")
	}
	if !strings.Contains(res.Message, "snapshot_name") {
		t.Errorf("失败原因应指向缺失的参数: %q", res.Message)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("缺少名字时不该调用 virsh，实际: %q", string(b))
	}
}

// TestSnapshotDeleteUsesSnapshotName 覆盖正常删除：`--snapshotname` 必须是
// 快照在虚拟化层里的名字，而不是用户填的显示名。
func TestSnapshotDeleteUsesSnapshotName(t *testing.T) {
	calls := snapshotFake(t, false, "demo-vm-snap-7\n")

	res, err := vmSnapshotDelete(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"snapshot_name": "demo-vm-snap-7"},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("删除应成功: err=%v res=%+v", err, res)
	}

	b, _ := os.ReadFile(calls)
	log := string(b)
	if !strings.Contains(log, "snapshot-delete") || !strings.Contains(log, "demo-vm-snap-7") {
		t.Errorf("应以快照名执行 snapshot-delete，实际: %q", log)
	}
	if !strings.Contains(log, "--snapshotname demo-vm-snap-7") {
		t.Errorf("缺 --snapshotname 参数: %q", log)
	}
}

// TestSnapshotDeleteLegacyDomainNameKey 覆盖兼容：老任务只带 domain_name。
func TestSnapshotDeleteLegacyDomainNameKey(t *testing.T) {
	calls := snapshotFake(t, false, "")

	res, err := vmSnapshotDelete(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"domain_name": "demo-vm-snap-9"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("旧键应被接受: %s", res.Message)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "demo-vm-snap-9") {
		t.Errorf("旧键未被使用: %q", string(b))
	}
}

// TestSnapshotDeleteMissingIsIdempotent 覆盖「快照确实不在了」。
//
// 幂等成功是对的：重试删除一个已删的快照不该报错。但必须**验证**它真的
// 不在了——清单里还有这个名字就说明删失败了。
func TestSnapshotDeleteMissingIsIdempotent(t *testing.T) {
	snapshotFake(t, true, "other-snap\n")

	res, err := vmSnapshotDelete(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"snapshot_name": "gone-snap"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("快照确实不存在时应幂等成功: %s", res.Message)
	}
}

// TestSnapshotDeleteStillPresentFails 覆盖「假成功」。
//
// virsh 说 not found，但清单里那个名字还在 → 删除并没有真的发生。旧实现
// 对任何含 "not find" 的错误一律放过，于是控制面删掉记录、节点上的快照
// 还在，而用户以为已经删了。
func TestSnapshotDeleteStillPresentFails(t *testing.T) {
	snapshotFake(t, true, "still-here\n")

	res, err := vmSnapshotDelete(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"snapshot_name": "still-here"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("快照仍在清单里时不能按成功处理（假成功会让控制面删掉记录）")
	}
	if !strings.Contains(res.Message, "删除快照失败") {
		t.Errorf("失败原因应指向删除失败: %q", res.Message)
	}
}
