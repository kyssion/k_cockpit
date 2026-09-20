package vm_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// seedPool 插入一个存储池。
func seedPool(t *testing.T, db *gorm.DB, nodeID int64, mount string) int64 {
	t.Helper()
	pool := model.StoragePool{
		NodeID: nodeID, DeviceID: "dev-" + mount, Kind: "local",
		Status: model.StoragePoolReady, MountPath: strPtrForTest(mount),
	}
	if err := db.Create(&pool).Error; err != nil {
		t.Fatalf("创建存储池失败: %v", err)
	}
	return pool.ID
}

func strPtrForTest(v string) *string { return &v }

// TestMigrateRejectsPoolOnAnotherNode 覆盖跨节点迁移。
//
// 磁盘是一份具体的文件，"迁到另一台机器的池里"等于让那台机器去读一个
// 不存在的路径——这类失败发生在搬了一半的时候，是最难收拾的一种。
func TestMigrateRejectsPoolOnAnotherNode(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-mig", OwnerID: ptr(int64(7))}
	db.Create(&row)
	other := seedPool(t, db, 2, "/mnt/other")

	_, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionMigrate, Dev: "vdb", TargetPoolID: other,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("跨节点迁移未被拒绝")
	}
	if !strings.Contains(err.Error(), "节点") {
		t.Errorf("错误未指出节点不匹配: %v", err)
	}
}

// TestMigrateRequiresTarget 覆盖"必须选目标"。
func TestMigrateRequiresTarget(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-mig2", OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionMigrate, Dev: "vdb",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("未选目标存储池却通过了")
	}
}

// TestMigrateRequiresHotConfirmation 覆盖运行中的热迁移确认。
//
// 热迁移期间磁盘仍在使用，业务会有抖动；是否接受只能由使用者判断，
// 服务端默认替他决定（无论是默认允许还是默认拒绝）都不合适。
func TestMigrateRequiresHotConfirmation(t *testing.T) {
	svc, queue, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	// 入队会校验任务类型是否已注册；本测试要走到"确认后受理"这一条路径。
	queue.Register(vm.NewDiskChangeExecutor(db, agent.NewMockClient()))
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-hot", OwnerID: ptr(int64(7))}
	db.Create(&row)
	pool := seedPool(t, db, 1, "/mnt/pool-a")

	_, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionMigrate, Dev: "vdb", TargetPoolID: pool,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("运行中未确认热迁移却通过了")
	}
	if !strings.Contains(err.Error(), "热迁移") {
		t.Errorf("错误未说明可以确认热迁移: %v", err)
	}

	// 明确确认后应当受理。
	if _, err := svc.ChangeDisk(ctx, row.ID, vm.DiskChangeRequest{
		Action: agent.DiskActionMigrate, Dev: "vdb", TargetPoolID: pool, AllowHot: true,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Errorf("确认热迁移后仍被拒绝: %v", err)
	}
}

// TestDisksReportsMigrateTargets 覆盖目标池的下发：没有第二个池时界面不该
// 显示迁移入口——一个点了必然失败的下拉框比没有这个按钮更糟。
func TestDisksReportsMigrateTargets(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-targets", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 测试环境自带一个默认池，因此这里断言"目标被下发且可迁移"。
	view, err := svc.Disks(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("读取磁盘列表失败: %v", err)
	}
	if len(view.MigrateTargets) == 0 {
		t.Fatal("存在可用存储池却未下发迁移目标")
	}
	if !view.Disks[0].CanMigrate {
		t.Errorf("有目标池却标记为不可迁移: %s", view.Disks[0].MigrateReason)
	}
}

// TestDisksHidesMigrationWithoutTarget 覆盖"没有第二个池就不给迁移入口"。
//
// 一个点了必然失败的下拉框比没有这个按钮更糟——用户会以为是自己选错了。
func TestDisksHidesMigrationWithoutTarget(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	// 换一个没有存储池的节点：ensureNodeUsable 只要求节点存在且不在维护模式。
	if err := db.Create(&model.Node{ID: 9, Name: "node-9"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	row := model.VM{NodeID: 9, Name: "vm-nopool", OwnerID: ptr(int64(7))}
	db.Create(&row)

	view, err := svc.Disks(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("读取磁盘列表失败: %v", err)
	}
	if len(view.MigrateTargets) != 0 {
		t.Fatalf("无存储池时目标数 = %d, 期望 0", len(view.MigrateTargets))
	}
	if view.Disks[0].CanMigrate {
		t.Error("没有目标池却标记为可迁移")
	}
	if view.Disks[0].MigrateReason == "" {
		t.Error("不可迁移却没有给出原因")
	}
}

// TestDeleteRejectsTransferForUnownedVM 覆盖「转移需要归属」。
//
// 无主的机器没有"我的存储"可转；默默地按保留处理会让用户以为磁盘已经
// 进了自己的文件列表——那比直接拒绝更糟。
func TestDeleteRejectsTransferForUnownedVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-orphan"}
	db.Create(&row)

	_, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionTransfer},
		authz.Viewer{UserID: 7, IsAdmin: true}, "root", "10.0.0.1")
	if err == nil {
		t.Fatal("无归属机器的转移未被拒绝")
	}
	if !strings.Contains(err.Error(), "归属") {
		t.Errorf("错误未说明缺少归属: %v", err)
	}
}
