package vm_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestCountIsCapped 覆盖数量上限。
//
// 理由要**说出来**而不只是拒绝：用户不知道为什么是 5。而这个理由是关于
// 存储 IO 的——同时进行的台数越多，宿主机上的存储被占得越久，表现为**所有**
// 虚拟机的 IO 都变慢，而用户很难把它和「我刚才点了克隆」联系起来。
func TestCountIsCapped(t *testing.T) {
	svc, _, db := newTestEnv(t)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)

	_, err := svc.BatchClone(context.Background(), vm.BatchCloneRequest{
		CreateRequest: vm.CreateRequest{NodeID: node.ID, VCPU: 2, MemoryMB: 2048, DiskGB: 20},
		NamePrefix:    "web", Count: vm.MaxBatchClone + 1,
	}, viewer7(), "alice", "10.0.0.1")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "IO") {
		t.Errorf("报错应说明理由（存储 IO），实际: %v", err)
	}
}

// TestWholeBatchIsCheckedForDuplicates 覆盖整批查重。
//
// **逐台跳过重名会建出带洞的结果**——比如只有 1、3、4 号，而用户看不出
// 少了哪一台，只会觉得"数量不对"。因此只要有一个名字被占用就整批拒绝。
func TestWholeBatchIsCheckedForDuplicates(t *testing.T) {
	svc, _, db := newTestEnv(t)
	owner := int64(7)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	// 占用 `web-2`，而这一批会生成 web-1 / web-2 / web-3。
	db.Create(&model.VM{
		Name: "web-2", NodeID: node.ID, Status: model.VMStatusStopped,
		CloneMode: model.CloneFull, OwnerID: &owner, DiskGB: 20,
	})

	_, err := svc.BatchClone(context.Background(), vm.BatchCloneRequest{
		CreateRequest: vm.CreateRequest{NodeID: node.ID, VCPU: 2, MemoryMB: 2048, DiskGB: 20},
		NamePrefix:    "web", Count: 3,
	}, viewer7(), "alice", "10.0.0.1")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "web-2") {
		t.Errorf("报错应指出是哪个名字被占了，实际: %v", err)
	}
	// 而且**不该建出任何一台**——部分建成才是"带洞"的来源。
	var n int64
	db.Model(&model.VM{}).Where("name LIKE ?", "web-%").Count(&n)
	if n != 1 {
		t.Errorf("整批拒绝后不该新增任何机器，实际共 %d 台", n)
	}
}

// TestInvalidPrefixIsRefusedEarly 覆盖前缀导致名称非法。
//
// 在**生成名称时**就拒绝，而不是等 Create 逐台失败——那样用户会看到
// 「1 台成功、2 台失败」，而原因是前缀本身，与"哪一台"无关。
func TestInvalidPrefixIsRefusedEarly(t *testing.T) {
	svc, _, db := newTestEnv(t)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)

	// 以连字符开头：生成的 `-1` 不合法（虚拟机名必须以字母或数字开头）。
	_, err := svc.BatchClone(context.Background(), vm.BatchCloneRequest{
		CreateRequest: vm.CreateRequest{NodeID: node.ID, VCPU: 2, MemoryMB: 2048, DiskGB: 20},
		NamePrefix:    "-bad", Count: 2,
	}, viewer7(), "alice", "10.0.0.1")
	assertStatus(t, err, 400)
	if err != nil && !strings.Contains(err.Error(), "前缀") {
		t.Errorf("报错应指向前缀，实际: %v", err)
	}
}

func TestZeroOrNegativeCountIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)

	for _, n := range []int{0, -1} {
		_, err := svc.BatchClone(context.Background(), vm.BatchCloneRequest{
			CreateRequest: vm.CreateRequest{NodeID: node.ID, VCPU: 2, MemoryMB: 2048, DiskGB: 20},
			NamePrefix:    "web", Count: n,
		}, viewer7(), "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("台数 %d 应被拒绝", n)
		}
	}
}

func TestEmptyPrefixIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)

	_, err := svc.BatchClone(context.Background(), vm.BatchCloneRequest{
		CreateRequest: vm.CreateRequest{NodeID: node.ID, VCPU: 2, MemoryMB: 2048, DiskGB: 20},
		NamePrefix:    "  ", Count: 2,
	}, viewer7(), "alice", "10.0.0.1")
	assertStatus(t, err, 400)
}
