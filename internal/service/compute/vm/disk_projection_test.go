package vm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// 本文件覆盖第 2 轮 NEW-001：disk_gb 投影与节点事实不符、且不会自愈。
//
// 真机证据（第 2 轮）：10 个并发 POST /vms/:id/disk/resize，任务 119/120/123
// 成功，节点 `qemu-img info` 报 11811160064 B（11 GiB），控制面 disk_gb=3。
// 三个根因分别对应下面三组用例：
//
//	入队预写           → TestResizeDiskDoesNotPreWriteProjection（见 disk_test.go）
//	执行器不回写       → TestDiskResizeExecutorPrefersNodeCapacity 等
//	采集器不对账容量   → monitor 包的 TestSyncRuntimeCorrectsDiskGB

// resizeTask 造一条与队列入队结果同形的扩容任务。
//
// 直接构造而不是走 Enqueue：这些用例要控制**旧容量**（模拟"扩容期间投影已被
// 别的路径改过"），而 Enqueue 会用当前 disk_gb 填进去。
func resizeTask(t *testing.T, vmID int64, params map[string]any) *model.Task {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("编码任务参数失败: %v", err)
	}
	s := string(raw)
	nodeID := int64(1)
	resourceType, resourceName := "vm", "vm-disk-test"
	resourceID := vmID
	return &model.Task{
		ID: 1, Type: model.TaskVMDiskResize, Status: model.TaskRunning,
		NodeID: &nodeID, ResourceType: &resourceType,
		ResourceID: &resourceID, ResourceName: &resourceName,
		Params: &s,
	}
}

// TestDiskResizeExecutorPrefersNodeCapacity 覆盖写回值的选择。
//
// **节点实测容量优先于请求值。** 请求值说的是"我想要多少"，实测值说的是
// "盘现在多大"——并发扩容时只有后者可信：10 个请求里只有第一个真正改变了
// 文件，后面的目标值都比文件当前容量还小。旧实现把请求值当成结果写下去，
// 于是控制面记着一个谁也没实现过的数字。
func TestDiskResizeExecutorPrefersNodeCapacity(t *testing.T) {
	_, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	client := newStubClient()
	client.exec = func(context.Context, agent.Operation) (*agent.Result, error) {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.VMDiskDataKey: agent.VMDiskResizeInfo{
				Applied: true, OldGB: 100, NewGB: 220,
				GuestGrowNeeded: true,
			},
		}}, nil
	}

	e := vm.NewDiskResizeExecutor(db, client)
	task := resizeTask(t, vid, map[string]any{
		"vm_id": vid, "vm_name": "vm-disk-test", "old_gb": 100, "new_gb": 200,
	})
	if err := e.Run(context.Background(), task); err != nil {
		t.Fatalf("扩容执行失败: %v", err)
	}

	var got model.VM
	if err := db.First(&got, vid).Error; err != nil {
		t.Fatalf("读回虚拟机失败: %v", err)
	}
	if got.DiskGB != 220 {
		t.Errorf("应按节点实测容量写回 220（请求值 200 不可信），实际 %d", got.DiskGB)
	}
}

// TestDiskResizeExecutorSkipsWriteWhenNotApplied 覆盖「节点说没扩」。
//
// Success 为 true 但 Applied 为 false 是一种真实组合：操作流程跑完了，但
// 镜像没有变大（例如节点侧的校验发现了别的问题而选择不动文件）。此时写投影
// 就等于把一次没发生的扩容记成事实——用户随后会来宾里找那块根本不存在的空间。
func TestDiskResizeExecutorSkipsWriteWhenNotApplied(t *testing.T) {
	_, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	client := newStubClient()
	client.exec = func(context.Context, agent.Operation) (*agent.Result, error) {
		return &agent.Result{
			Success: true, Message: "镜像未扩大，已放弃",
			Data: map[string]any{
				agent.VMDiskDataKey: agent.VMDiskResizeInfo{Applied: false, OldGB: 100},
			},
		}, nil
	}

	e := vm.NewDiskResizeExecutor(db, client)
	task := resizeTask(t, vid, map[string]any{
		"vm_id": vid, "vm_name": "vm-disk-test", "old_gb": 100, "new_gb": 200,
	})
	err := e.Run(context.Background(), task)
	assertStatus(t, err, 422)

	var got model.VM
	if err := db.First(&got, vid).Error; err != nil {
		t.Fatalf("读回虚拟机失败: %v", err)
	}
	if got.DiskGB != 100 {
		t.Errorf("未生效时不得改动投影，实际 %d", got.DiskGB)
	}
}

// TestDiskResizeWriteBackKeepsChangedRow 覆盖条件更新。
//
// 扩容的这段时间里 disk_gb 可能已经被别的路径改过——采集器的域扫描对账出
// 真实容量，或用户又发起过一次扩容。无条件 Update 会把这些更正确的值覆盖成
// 一个基于旧假设（old_gb）的数字，于是一次已经完成的扩容反而把投影改错。
//
// RowsAffected == 0 时不覆盖，任务仍算成功：节点上那张盘确实变大了，把任务
// 判失败会让用户以为没扩从而再操作一次。
func TestDiskResizeWriteBackKeepsChangedRow(t *testing.T) {
	_, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	// 模拟"扩容期间投影被另一条路径改成了 150"。
	if err := db.Model(&model.VM{}).Where("id = ?", vid).
		Update("disk_gb", 150).Error; err != nil {
		t.Fatalf("预置容量失败: %v", err)
	}

	client := newStubClient()
	client.exec = func(context.Context, agent.Operation) (*agent.Result, error) {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.VMDiskDataKey: agent.VMDiskResizeInfo{
				Applied: true, OldGB: 100, NewGB: 200,
			},
		}}, nil
	}

	// 任务参数里的旧容量是 100，与当前行（150）对不上。
	e := vm.NewDiskResizeExecutor(db, client)
	task := resizeTask(t, vid, map[string]any{
		"vm_id": vid, "vm_name": "vm-disk-test", "old_gb": 100, "new_gb": 200,
	})
	if err := e.Run(context.Background(), task); err != nil {
		t.Fatalf("节点已扩容时任务不应失败: %v", err)
	}

	var got model.VM
	if err := db.First(&got, vid).Error; err != nil {
		t.Fatalf("读回虚拟机失败: %v", err)
	}
	if got.DiskGB != 150 {
		t.Errorf("条件不符时不得覆盖（留给采集器收敛），实际 %d", got.DiskGB)
	}
}

// TestResizeDiskRejectsWhileOneIsInFlight 覆盖入口互斥。
//
// 队列的资源锁只保证同资源的任务**串行执行**，不阻止它们被创建。于是 10 个
// 并发请求各自拿到"已受理"，实际只有第一个能改变容量——用户看到 10 个任务
// 在排队，而其中 9 个必然落空（真机上就是这样产生 3 GB 那个数字的）。
//
// 因此入口必须在有在途任务时直接拒绝，并且**不再入队**。
func TestResizeDiskRejectsWhileOneIsInFlight(t *testing.T) {
	svc, q, db := newTestEnv(t)
	q.Register(vm.NewDiskResizeExecutor(db, newStubClient()))
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	if _, err := svc.ResizeDisk(context.Background(), vid, 200,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("第一次扩容应被受理: %v", err)
	}

	_, err := svc.ResizeDisk(context.Background(), vid, 300,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "扩容任务进行中") {
		t.Errorf("文案应说明是并发扩容，实际: %v", err)
	}

	var n int64
	if err := db.Model(&model.Task{}).
		Where("type = ? AND resource_type = ? AND resource_id = ?",
			model.TaskVMDiskResize, "vm", vid).
		Count(&n).Error; err != nil {
		t.Fatalf("统计任务失败: %v", err)
	}
	if n != 1 {
		t.Errorf("在途期间不应再入队：任务数 = %d，期望 1", n)
	}
}
