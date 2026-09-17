package vm_test

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// reinstallTemplate 建一个可用于重装的模板。
func reinstallTemplate(t *testing.T, db *gorm.DB, nodeID int64, name string, owner int64) int64 {
	t.Helper()
	path := "/tpl/" + name + ".qcow2"
	tpl := model.Template{
		NodeID: nodeID, Name: name, Status: model.TemplateReady,
		CloneEnabled: true, DiskPath: &path, MinDiskGB: 20, CreatedBy: &owner,
	}
	if err := db.Create(&tpl).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}
	return tpl.ID
}

func TestReinstallRequiresStopped(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-ri1", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)
	tplID := reinstallTemplate(t, db, 1, "tpl-ri1", 7)

	_, err := svc.Reinstall(ctx, row.ID, tplID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// TestReinstallBacksUpPreviousSystemDisk 覆盖 F-2-11 的核心承诺：
// 备份原系统盘，且**重装成功后备份依然存在**。
//
// 备份是用户「回到原来的系统」的唯一退路。重装一成功就删掉它，等于替用户
// 决定「你不需要退路了」——而重装往往正是在系统已经出问题的时候做的，
// 那时人比平时更可能想反悔。
func TestReinstallBacksUpPreviousSystemDisk(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{
		NodeID: 1, Name: "vm-ri2", Status: model.VMStatusStopped,
		DiskGB: 40, OwnerID: ptr(int64(7)), Present: true,
	}
	db.Create(&row)
	tplID := reinstallTemplate(t, db, 1, "tpl-ri2", 7)

	if _, err := svc.Reinstall(ctx, row.ID, tplID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitForVM(t, svc, row.ID, viewer, func(v vm.View) bool { return v.ReinstallAt != nil })

	view, err := svc.Get(ctx, row.ID, viewer)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !view.HasReinstallBackup {
		t.Error("重装后没有留下备份——用户失去了回到原系统的唯一退路")
	}
	if view.TemplateID == nil || *view.TemplateID != tplID {
		t.Errorf("未记录重装所用模板: %v", view.TemplateID)
	}

	// 备份路径本身**不下发**：它是宿主机上的内部细节，暴露出去会诱使用户
	// 去宿主机上直接操作那个文件。
	var stored model.VM
	db.First(&stored, row.ID)
	if stored.ReinstallBackup == nil || *stored.ReinstallBackup == "" {
		t.Error("数据库中未记录备份路径")
	}
}

// TestReinstallRejectedWhenBackupExists 覆盖一条会让人后悔很久的规则。
//
// 只有一份备份，第二次重装会覆盖上一次的。静默覆盖会让用户失去「回到上一个
// 系统」这个唯一的退路——而他可能正指望那条退路。因此显式拒绝。
func TestReinstallRejectedWhenBackupExists(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	backup := "/var/lib/k_cockpit/backups/vm-ri3.qcow2.bak"
	row := model.VM{
		NodeID: 1, Name: "vm-ri3", Status: model.VMStatusStopped,
		DiskGB: 40, OwnerID: ptr(int64(7)), Present: true, ReinstallBackup: &backup,
	}
	db.Create(&row)
	tplID := reinstallTemplate(t, db, 1, "tpl-ri3", 7)

	_, err := svc.Reinstall(ctx, row.ID, tplID, viewer, "alice", "10.0.0.1")
	assertAPIError(t, err, 409)
}

// TestPurgeBackupRequiresStopped 覆盖清理备份的前置条件。
//
// 运行中不允许清理：节点侧系统盘可能是从备份派生的 overlay，
// 删掉它会让运行中的虚拟机在读到某块未缓存的数据时崩掉。
func TestPurgeBackupRequiresStopped(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	backup := "/var/lib/k_cockpit/backups/vm-ri4.qcow2.bak"
	row := model.VM{
		NodeID: 1, Name: "vm-ri4", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), ReinstallBackup: &backup,
	}
	db.Create(&row)

	_, err := svc.PurgeBackup(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestPurgeBackupClearsRecord(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	backup := "/var/lib/k_cockpit/backups/vm-ri5.qcow2.bak"
	row := model.VM{
		NodeID: 1, Name: "vm-ri5", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), Present: true, ReinstallBackup: &backup,
	}
	db.Create(&row)

	if _, err := svc.PurgeBackup(ctx, row.ID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitForVM(t, svc, row.ID, viewer, func(v vm.View) bool { return !v.HasReinstallBackup })

	var stored model.VM
	db.First(&stored, row.ID)
	if stored.ReinstallBackup != nil {
		t.Errorf("备份记录未清空: %v", *stored.ReinstallBackup)
	}
}

func TestPurgeBackupRejectedWhenNoBackup(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-ri6", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.PurgeBackup(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestReinstallRejectsTemplateOnAnotherNode(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-ri7", Status: model.VMStatusStopped,
		DiskGB: 20, OwnerID: ptr(int64(7)), Present: true,
	}
	db.Create(&row)
	// 模板在另一个节点上。
	tplID := reinstallTemplate(t, db, 99, "tpl-ri7", 7)

	_, err := svc.Reinstall(ctx, row.ID, tplID, authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)
}

func TestReinstallRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-ri8", Status: model.VMStatusStopped,
		DiskGB: 20, OwnerID: ptr(int64(20)), Present: true,
	}
	db.Create(&row)
	tplID := reinstallTemplate(t, db, 1, "tpl-ri8", 20)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.Reinstall(ctx, row.ID, tplID, authz.Viewer{UserID: 10}, "bob", "")
	assertAPIError(t, err, 404)
}
