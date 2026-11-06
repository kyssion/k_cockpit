package vm_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// 本文件覆盖第 6 轮 DEF-R6-02：卸载一个不存在的磁盘仍返回 200 + success。
//
// 真机实测：`DELETE` 一个不存在的设备 → `200 success`，`dumpxml` 前后都是
// `['vda']`（静默无操作）。与已修的 DEF-R3-09（卸载不存在的 shares tag → 404）
// 是同一类问题：用户无法分辨「已经卸掉」与「本来就没有」，而这两种情况该做
// 的事完全不同（一个该去挂盘，一个该去查为什么没生效）。
//
// 这里同时钉住**正向对照**：真实存在的数据盘仍然能正常卸载——别把它修成
// 一律 404。

// diskAgent 让 vm.disk.list 返回指定的磁盘清单。
type diskAgent struct {
	*stubClient
	disks []agent.VMDisk
}

func (d *diskAgent) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind == agent.OpVMDiskList {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.VMDiskListDataKey: d.disks,
		}}, nil
	}
	return d.stubClient.Execute(ctx, op)
}

// detachEnv 造一台带 vda(系统盘) + vdb(数据盘) 的关机虚拟机。
func detachEnv(t *testing.T) (*vm.Service, int64, authz.Viewer) {
	t.Helper()
	client := &diskAgent{
		stubClient: newStubClient(),
		disks: []agent.VMDisk{
			{Dev: "vda", IsSystem: true, Bus: "virtio", CapacityGB: 20},
			{Dev: "vdb", IsSystem: false, Bus: "virtio", CapacityGB: 100, Hotpluggable: true},
		},
	}
	svc, queue, db := newTestEnvWithClient(t, client)
	// 注册磁盘变更执行器：否则入队会以"不支持的任务类型"失败，正向对照就
	// 变成了在测队列，而不是在测"存在的设备能不能卸"。
	queue.Register(vm.NewDiskChangeExecutor(db, client))
	id := seedVM(t, db, 20, model.VMStatusStopped)
	return svc, id, authz.Viewer{UserID: 7, IsAdmin: true}
}

func detachReq(dev string) vm.DiskChangeRequest {
	return vm.DiskChangeRequest{Action: agent.DiskActionDetach, Dev: dev}
}

// TestDetachMissingDiskRejected 是 DEF-R6-02 的回归断言。
func TestDetachMissingDiskRejected(t *testing.T) {
	svc, id, viewer := detachEnv(t)
	_, err := svc.ChangeDisk(context.Background(), id, detachReq("vdz"), viewer, "admin", "10.0.0.1")
	assertStatus(t, err, 404)
}

// TestDetachExistingDiskSucceeds 是正向对照：真实存在的数据盘仍能卸载。
func TestDetachExistingDiskSucceeds(t *testing.T) {
	svc, id, viewer := detachEnv(t)
	task, err := svc.ChangeDisk(context.Background(), id, detachReq("vdb"), viewer, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("卸载存在的 vdb 不该失败: %v", err)
	}
	if task == nil || task.ID == 0 {
		t.Fatal("应入队一个卸载任务")
	}
}

// TestDetachSystemDiskStillRejected 覆盖既有守卫没被这次改动弄坏。
func TestDetachSystemDiskStillRejected(t *testing.T) {
	svc, id, viewer := detachEnv(t)
	_, err := svc.ChangeDisk(context.Background(), id, detachReq("vda"), viewer, "admin", "10.0.0.1")
	assertStatus(t, err, 422)
}
