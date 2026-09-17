package vm_test

import (
	"context"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// waitForVM 轮询直到详情满足条件。
//
// 救援的执行器是异步的（走队列），受理成功后记录不会立刻变成目标状态——
// 直接断言会读到「还没开始」的中间态，而那个状态下的字段同样有可能满足
// 一个写错的断言，测试就变成了碰运气。
func waitForVM(
	t *testing.T, svc *vm.Service, id int64, viewer authz.Viewer, cond func(vm.View) bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := svc.Get(context.Background(), id, viewer)
		if err == nil && cond(*v) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待虚拟机 %d 达到目标状态超时", id)
}

func TestEnterRescueRequiresStopped(t *testing.T) {
	// 探测返回 running：救援要改硬件配置，热改会让控制面记录的配置与
	// 虚拟化层实际的配置分叉——而那份分叉在退出还原时会变成一个
	// 「还原成什么样」说不清的状态。
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-r1", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.EnterRescue(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// TestRescueRoundTripRestoresConfig 覆盖 F-2-12 的核心承诺：**退出还原**。
//
// 进入前把配置存下来，退出时按它写回。没有这一步就只能猜一个默认值填回去，
// 那是悄悄改掉用户的配置——他进入救援是为了修系统，退出后发现引导顺序被
// 重置成了默认值，机器起不来了。
func TestRescueRoundTripRestoresConfig(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	original := model.VM{
		NodeID: 1, Name: "vm-rescue", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)),
		// 一组**刻意非默认**的配置：用默认值做样本的话，即使还原逻辑
		// 写的是「填默认值」也会通过，测试就失去意义了。
		BootOrder:     "cdrom,disk",
		MachineType:   "i440fx",
		DisplayDevice: "vnc",
		Firmware:      "bios",
		SecureBoot:    false,
	}
	db.Create(&original)

	// --- 进入救援 ---
	if _, err := svc.EnterRescue(ctx, original.ID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("进入救援失败: %v", err)
	}
	waitForVM(t, svc, original.ID, viewer, func(v vm.View) bool { return v.RescueActive })

	entered, err := svc.Get(ctx, original.ID, viewer)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !entered.RescueActive {
		t.Fatal("未标记为救援模式")
	}
	if entered.RescueSince == nil {
		t.Error("未记录进入救援的时间")
	}

	var stored model.VM
	if err := db.First(&stored, original.ID).Error; err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if stored.RescueConfig == nil || *stored.RescueConfig == "" {
		t.Fatal("未保存配置快照——退出时将无法还原")
	}

	// --- 退出救援 ---
	if _, err := svc.ExitRescue(ctx, original.ID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("退出救援失败: %v", err)
	}
	waitForVM(t, svc, original.ID, viewer, func(v vm.View) bool { return !v.RescueActive })

	var restored model.VM
	if err := db.First(&restored, original.ID).Error; err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if restored.BootOrder != original.BootOrder {
		t.Errorf("引导顺序未还原: %q → %q", original.BootOrder, restored.BootOrder)
	}
	if restored.MachineType != original.MachineType {
		t.Errorf("机型未还原: %q → %q", original.MachineType, restored.MachineType)
	}
	if restored.Firmware != original.Firmware {
		t.Errorf("固件未还原: %q → %q", original.Firmware, restored.Firmware)
	}
	// 快照要一起清掉：留着一份不再对应的快照，下次进入救援时若那次进入
	// 失败，旧的会被当成「本次的原配置」去还原，还原到一个更久远的状态。
	if restored.RescueConfig != nil {
		t.Error("退出后快照未清除")
	}
	if restored.RescueSince != nil {
		t.Error("退出后救援时刻未清除")
	}
}

func TestEnterRescueRejectsWhenAlreadyActive(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{
		NodeID: 1, Name: "vm-r2", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), RescueActive: true,
	}
	db.Create(&row)

	_, err := svc.EnterRescue(ctx, row.ID, viewer, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestExitRescueRejectsWhenNotInRescue(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-r3", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.ExitRescue(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// TestExitRescueWithoutSnapshotIsRejected 覆盖一条刻意的选择。
//
// 快照缺失时**拒绝退出**，而不是用默认值「还原」：后者会静默改掉用户的
// 引导顺序与固件设置，而他无从判断这是不是自己原来的配置。相比起来，
// 「需要人工确认」是一个明确得多、也安全得多的结果。
func TestExitRescueWithoutSnapshotIsRejected(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	// 状态是「救援中」但没有快照——模拟历史数据或写入中断。
	row := model.VM{
		NodeID: 1, Name: "vm-r4", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), RescueActive: true,
	}
	db.Create(&row)

	_, err := svc.ExitRescue(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestRescueRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	theirs := model.VM{
		NodeID: 1, Name: "vm-r5", Status: model.VMStatusStopped, OwnerID: ptr(int64(20)),
	}
	db.Create(&theirs)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.EnterRescue(ctx, theirs.ID, authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)
}
