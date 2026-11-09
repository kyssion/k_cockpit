package vm_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/service/compute/vm"
)

// snapshotClient 捕获 vm.snapshot.* 的下发参数，并可让 create 失败。
type snapshotClient struct {
	*stubClient
	mu         sync.Mutex
	ops        []agent.Operation
	failCreate bool
	// down 让快照类指令"送不到节点"（Dispatch 返回 error）。
	down bool
}

func (c *snapshotClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	switch op.Kind {
	case agent.OpVMSnapshotCreate, agent.OpVMSnapshotDelete:
		if c.down {
			return nil, errors.New("node unreachable")
		}
		c.mu.Lock()
		c.ops = append(c.ops, op)
		c.mu.Unlock()
		if op.Kind == agent.OpVMSnapshotCreate {
			if c.failCreate {
				// 与实机一致的形状：UEFI 域不支持内部快照，节点明确拒绝。
				return &agent.Result{Success: false,
					Message: "创建快照失败: internal snapshots of a VM with pflash based firmware are not supported"}, nil
			}
			return &agent.Result{Success: true, Data: map[string]any{
				"domain_name":   "kc-snap-1",
				"snapshot_kind": model.SnapshotKindExternal,
			}}, nil
		}
		return &agent.Result{Success: true}, nil
	default:
		return c.stubClient.Execute(ctx, op)
	}
}

func (c *snapshotClient) opCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ops)
}

func (c *snapshotClient) opOf(t *testing.T, kind agent.OpKind) agent.Operation {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, op := range c.ops {
		if op.Kind == kind {
			return op
		}
	}
	t.Fatalf("没有下发 %v", kind)
	return agent.Operation{}
}

// snapshotEnv 造一台虚拟机，并注册快照执行器。
func snapshotEnv(t *testing.T, client *snapshotClient) (*vm.Service, *gorm.DB, int64) {
	t.Helper()
	svc, q, db := newTestEnvWithClient(t, client)
	q.Register(vm.NewSnapshotCreateExecutor(db, client))
	q.Register(vm.NewSnapshotDeleteExecutor(db, client))

	owner := int64(7)
	row := model.VM{NodeID: 1, Name: "snap-vm", Status: model.VMStatusStopped,
		OwnerID: &owner, DiskGB: 20, Present: true, CloneMode: model.CloneFull}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return svc, db, row.ID
}

// TestSnapshotDeleteSendsSnapshotName 覆盖「删除快照传错参数键」。
//
// 实机现象：删除快照任务 failed，`virsh: error: Failed to get option
// 'snapshotname': Option argument is empty`——执行器只下发了 domain_name，
// 而节点读的是 snapshot_name，于是恒为空、删除从未真正执行。
//
// 更坏的是旧实现「not find 一律按成功」：名字恰好不对时**假成功**，
// 控制面删了记录、节点上的快照还在。
func TestSnapshotDeleteSendsSnapshotName(t *testing.T) {
	client := &snapshotClient{stubClient: newStubClient()}
	svc, db, vmID := snapshotEnv(t, client)
	ctx := context.Background()

	domainName := "kc-snap-42"
	snap := model.VMSnapshot{VMID: vmID, NodeID: 1, Name: "before-upgrade",
		Status: model.SnapshotReady, Kind: model.SnapshotKindInternal,
		DomainName: &domainName}
	if err := db.Create(&snap).Error; err != nil {
		t.Fatalf("创建快照记录失败: %v", err)
	}

	if _, err := svc.DeleteSnapshot(ctx, vmID, snap.ID, viewer7(), "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	// 等执行器真正把指令发出去（队列有 20ms 轮询间隔）。
	waitFor(t, "快照删除指令下发", func() bool { return client.opCount() > 0 })
	op := client.opOf(t, agent.OpVMSnapshotDelete)
	if got, _ := op.Params["snapshot_name"].(string); got != domainName {
		t.Errorf("下发参数 snapshot_name = %v, 期望 %q（节点按它执行 --snapshotname，空了就必然失败）",
			op.Params["snapshot_name"], domainName)
	}
	if got, _ := op.Params["domain_name"].(string); got != domainName {
		t.Errorf("兼容旧键 domain_name = %v, 期望 %q", got, domainName)
	}
}

// TestSnapshotCreateFailureRemovesRecord 覆盖「失败后留下 ready 幽灵记录」。
//
// 实机现象：UEFI 域 + include_memory=true 的快照任务 failed（错误可读），
// 但列表里仍有一条 status=ready 的记录，can_restore / can_delete 都是 true
// ——用户点恢复会失败、点删除也会失败（见「删除快照传错参数键」）。
//
// 一条指向「从未存在过的快照」的记录比没有记录更糟：它让人以为数据还在。
func TestSnapshotCreateFailureRemovesRecord(t *testing.T) {
	client := &snapshotClient{stubClient: newStubClient(), failCreate: true}
	svc, db, vmID := snapshotEnv(t, client)

	if _, err := svc.CreateSnapshot(context.Background(), vmID, vm.CreateSnapshotRequest{
		Name: "uefi-mem", IncludeMemory: true,
	}, viewer7(), "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, "失败的快照记录被清理", func() bool {
		var n int64
		db.Model(&model.VMSnapshot{}).Where("vm_id = ?", vmID).Count(&n)
		return n == 0
	})
}

// TestSnapshotCreateUnreachableKeepsErrorRecord 覆盖反面：节点不可达时**不删**
// 记录。
//
// 那种情况"到底建没建成功"说不准——节点上可能真的有一份快照。删掉记录会
// 让那份快照永久失去管理入口，因此保留为 error 态，等对账或用户显式删除。
func TestSnapshotCreateUnreachableKeepsErrorRecord(t *testing.T) {
	client := &snapshotClient{stubClient: newStubClient(), down: true}
	svc, db, vmID := snapshotEnv(t, client)

	if _, err := svc.CreateSnapshot(context.Background(), vmID, vm.CreateSnapshotRequest{
		Name: "lost-node",
	}, viewer7(), "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, "快照记录进入 error 态", func() bool {
		var row model.VMSnapshot
		if err := db.Where("vm_id = ?", vmID).First(&row).Error; err != nil {
			return false
		}
		return row.Status == model.SnapshotError
	})
}
