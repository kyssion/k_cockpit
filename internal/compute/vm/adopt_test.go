package vm_test

import (
	"context"
	"testing"

	"k_cockpit/internal/compute/vm"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
)

// TestForceDeleteSkipsStatusProbe 强删的核心价值：探测为运行态（普通删除
// 必拒）的机器仍可受理，且任务完成后记录标记为已删除。
func TestForceDeleteSkipsStatusProbe(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	admin := authz.Viewer{UserID: 1, IsAdmin: true}

	// mock 对没见过的机器探测为 running：普通删除应当被拒，
	// 这正是「僵尸机删不掉」的形态。
	row := &model.VM{
		NodeID: 1, Name: "zombie-1", Status: model.VMStatusError,
		VCPU: 2, MemoryMB: 2048, DiskGB: 40,
		CloneMode: model.CloneFull, Present: true,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("插入虚拟机失败: %v", err)
	}

	_, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionKeep},
		admin, "admin", "127.0.0.1")
	if err == nil {
		t.Fatal("普通删除对运行态机器应当被拒绝，实际成功")
	}

	task, err := svc.ForceDelete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionKeep},
		admin, "admin", "127.0.0.1")
	if err != nil {
		t.Fatalf("强制删除应当跳过状态探测直接受理: %v", err)
	}
	waitFor(t, "强删任务完成", func() bool {
		var tk model.Task
		if err := db.First(&tk, task.ID).Error; err != nil {
			return false
		}
		return tk.Status == model.TaskSuccess || tk.Status == model.TaskFailed
	})

	var after model.VM
	if err := db.First(&after, row.ID).Error; err != nil {
		t.Fatalf("查询删除后的记录失败: %v", err)
	}
	if after.Present {
		t.Error("强删完成后记录应标记 present=false")
	}
}

// TestForceDeleteRespectsLock 锁在强删路径上仍然拦：先解锁（独立的高风险
// 动作）再强删，两次决定各自留痕。
func TestForceDeleteRespectsLock(t *testing.T) {
	svc, _, db := newTestEnv(t)
	admin := authz.Viewer{UserID: 1, IsAdmin: true}

	row := &model.VM{
		NodeID: 1, Name: "zombie-locked", Status: model.VMStatusError,
		VCPU: 2, MemoryMB: 2048, DiskGB: 40,
		CloneMode: model.CloneFull, Present: true,
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("插入虚拟机失败: %v", err)
	}
	reason := "prod"
	if err := db.Create(&model.VMLock{VMID: row.ID, Locked: true, Reason: &reason}).Error; err != nil {
		t.Fatalf("插入锁记录失败: %v", err)
	}

	_, err := svc.ForceDelete(context.Background(), row.ID,
		vm.DeleteRequest{DiskAction: vm.DiskActionKeep}, admin, "admin", "127.0.0.1")
	assertAPIError(t, err, 422)
}

// TestUnmanagedDomainsFiltersKnown 扫描结果要过滤掉已有记录的域名：
// 不过滤的话，每台已纳管的机器都会出现在「未纳管」列表里。
func TestUnmanagedDomainsFiltersKnown(t *testing.T) {
	svc, _, db := newTestEnv(t)
	if err := db.Create(&model.VM{
		NodeID: 1, Name: "legacy-ubuntu-1804", Status: model.VMStatusRunning,
		VCPU: 2, MemoryMB: 4096, DiskGB: 60, CloneMode: model.CloneFull, Present: true,
	}).Error; err != nil {
		t.Fatalf("插入已有记录失败: %v", err)
	}

	items, err := svc.UnmanagedDomains(context.Background(), 1,
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("扫描未纳管域失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("预置两台、已有一台记录，应只剩 1 台未纳管，实际 %d 台", len(items))
	}
	if items[0].Name != "bare-win2022" {
		t.Errorf("剩余的未纳管域应为 bare-win2022，实际 %s", items[0].Name)
	}
}

// TestAdoptDomainCreatesRecord 纳管以扫描结果为准建立记录：规格来自域
// 定义，重复纳管被拒，纳管不存在的域被拒。
func TestAdoptDomainCreatesRecord(t *testing.T) {
	svc, _, _ := newTestEnv(t)
	ctx := context.Background()
	admin := authz.Viewer{UserID: 1, IsAdmin: true}

	view, err := svc.AdoptDomain(ctx, 1, vm.AdoptRequest{Domain: "bare-win2022"},
		admin, "admin", "127.0.0.1")
	if err != nil {
		t.Fatalf("纳管失败: %v", err)
	}
	if view.Name != "bare-win2022" || view.VCPU != 4 || view.MemoryMB != 8192 {
		t.Errorf("纳管记录应使用域定义的规格，实际 name=%s vcpu=%d mem=%d",
			view.Name, view.VCPU, view.MemoryMB)
	}
	if view.Status != model.VMStatusStopped {
		t.Errorf("纳管记录的状态应来自扫描结果（stopped），实际 %s", view.Status)
	}
	if view.UUID == "" {
		// UUID 由 OpVMAdopt 回传并记录为对账键；缺失不是错误（节点指令
		// 允许送达失败），但 mock 正常时应带回。
		t.Error("纳管记录应带回节点侧的域 UUID")
	}

	// 再纳一次：记录已存在，应当以冲突拒绝而不是建出第二条。
	_, err = svc.AdoptDomain(ctx, 1, vm.AdoptRequest{Domain: "bare-win2022"},
		admin, "admin", "127.0.0.1")
	assertAPIError(t, err, 409)

	// 纳管不存在的域：不建「永远状态未知」的空记录。
	_, err = svc.AdoptDomain(ctx, 1, vm.AdoptRequest{Domain: "no-such-domain"},
		admin, "admin", "127.0.0.1")
	assertAPIError(t, err, 422)
}
