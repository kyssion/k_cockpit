package vm_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

func TestCreateRejectsUnreadyTemplate(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		status string
		enable bool
	}{
		// 制备中与失败要**分开说**：前者要等，后者要重建。都说成「不可用」
		// 会让用户一直等一个永远不会就绪的模板。
		{"制备中", model.TemplatePreparing, true},
		{"制备失败", model.TemplateFailed, true},
		{"已停止提供克隆", model.TemplateReady, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := int64(7)
			path := "/tpl/" + tc.status + ".qcow2"
			tpl := model.Template{
				NodeID: 1, Name: "tpl-" + tc.status + "-" + boolTag(tc.enable),
				Status:   tc.status,
				DiskPath: &path, MinDiskGB: 20, CreatedBy: &owner,
			}
			if err := db.Create(&tpl).Error; err != nil {
				t.Fatalf("创建模板失败: %v", err)
			}

			// 关闭克隆必须走 Update。
			//
			// ⚠️ `CloneEnabled` 带 `default:true`，而 GORM 在 Create 时会
			// **省略零值**（false），数据库随即填入 `true`——直接
			// `Create(&Template{CloneEnabled: false})` 得到的是一条**允许克隆**
			// 的记录。这与 model/schedule.go 里记录的是同一个陷阱。
			//
			// 走 Update 也更贴近真实路径：关闭克隆本来就是一个后续动作，
			// 不会在创建模板的那一刻就决定。
			if !tc.enable {
				if err := db.Model(&model.Template{}).Where("id = ?", tpl.ID).
					Update("clone_enabled", false).Error; err != nil {
					t.Fatalf("关闭克隆失败: %v", err)
				}
			}

			_, err := svc.Create(ctx, vm.CreateRequest{
				Name: "clone-x", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
				TemplateID: tpl.ID, CloneMode: model.CloneFull,
			}, authz.Viewer{UserID: owner}, "alice", "10.0.0.1")
			if err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
}

// TestCreateRejectsTemplateOnAnotherNode 覆盖一条会让人困惑很久的失败。
//
// 模板盘就在它所属节点的存储池里。允许跨节点使用会让节点去挂载一个不存在
// 的路径，报错通常是一句「file not found」——从它出发几乎不可能定位到
// 「你选的模板在另一台机器上」。
func TestCreateRejectsTemplateOnAnotherNode(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	owner := int64(7)
	path := "/tpl/other.qcow2"
	tpl := model.Template{
		NodeID: 99, Name: "tpl-other", Status: model.TemplateReady,
		CloneEnabled: true, DiskPath: &path, MinDiskGB: 20, CreatedBy: &owner,
	}
	db.Create(&tpl)

	_, err := svc.Create(ctx, vm.CreateRequest{
		Name: "clone-y", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		TemplateID: tpl.ID, CloneMode: model.CloneFull,
	}, authz.Viewer{UserID: owner}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// TestCreateRejectsBadCloneMode 确认链式克隆必须是**显式且合法**的选择。
func TestCreateRejectsBadCloneMode(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	owner := int64(7)
	path := "/tpl/mode.qcow2"
	tpl := model.Template{
		NodeID: 1, Name: "tpl-mode", Status: model.TemplateReady,
		CloneEnabled: true, DiskPath: &path, MinDiskGB: 20, CreatedBy: &owner,
	}
	db.Create(&tpl)

	_, err := svc.Create(ctx, vm.CreateRequest{
		Name: "clone-z", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		TemplateID: tpl.ID, CloneMode: "snapshot",
	}, authz.Viewer{UserID: owner}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

// waitForVMNamed 等待创建任务落库，返回该虚拟机的记录。
//
// 创建是异步的（走队列），记录由执行器在**节点成功之后**写入，因此不能
// 受理完就查——那时它还不存在。
func waitForVMNamed(t *testing.T, db *gorm.DB, name string) model.VM {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var row model.VM
		if err := db.Where("name = ?", name).First(&row).Error; err == nil {
			return row
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待虚拟机 %q 落库超时", name)
	return model.VM{}
}

// TestCloneLinkedRecordsBackingPath 覆盖链式克隆的依赖记录。
//
// 依赖链的存在与否决定了删除模板时该不该拒绝——记不下来，删除检查就会
// 放过一个正在被依赖的模板，而下游的虚拟机要到开机时才出问题。
func TestCloneLinkedRecordsBackingPath(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	path := "/tpl/linked.qcow2"
	tpl := model.Template{
		NodeID: 1, Name: "tpl-linked", Status: model.TemplateReady,
		CloneEnabled: true, DiskPath: &path, MinDiskGB: 20,
		DefaultCPU: 4, DefaultMemoryMB: 4096, CreatedBy: ptr(int64(7)),
	}
	db.Create(&tpl)

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "clone-linked", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 10,
		TemplateID: tpl.ID, CloneMode: model.CloneLinked,
	}, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	created := waitForVMNamed(t, db, "clone-linked")
	if created.CloneMode != model.CloneLinked {
		t.Errorf("克隆方式 = %q, 期望 linked", created.CloneMode)
	}
	if created.TemplateID == nil || *created.TemplateID != tpl.ID {
		t.Error("未记录来源模板")
	}
	if created.BackingPath == nil || *created.BackingPath != path {
		t.Errorf("未记录父盘路径: %v", created.BackingPath)
	}
}

// TestCloneFullHasNoBackingDependency 确认完整克隆**不**留下依赖。
//
// 留下一条不存在的依赖会让界面上显示出一个假的依赖链——而依赖链的存在
// 与否决定了删除模板时该不该拒绝：一个假依赖会让本该能删的模板删不掉。
func TestCloneFullHasNoBackingDependency(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	path := "/tpl/full.qcow2"
	tpl := model.Template{
		NodeID: 1, Name: "tpl-full", Status: model.TemplateReady,
		CloneEnabled: true, DiskPath: &path, MinDiskGB: 20, CreatedBy: ptr(int64(7)),
	}
	db.Create(&tpl)

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "clone-full", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		TemplateID: tpl.ID, CloneMode: model.CloneFull,
	}, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	created := waitForVMNamed(t, db, "clone-full")
	if created.CloneMode != model.CloneFull {
		t.Errorf("克隆方式 = %q, 期望 full", created.CloneMode)
	}
	if created.BackingPath != nil {
		t.Errorf("完整克隆不应有父盘依赖: %v", *created.BackingPath)
	}
}

// TestCloneRaisesDiskToTemplateMinimum 覆盖一条会产生**误导性报错**的校验。
//
// overlay 建在比父盘小的空间上会直接失败，而节点侧的报错通常是一句
// 「write beyond end of device」——从它出发几乎不可能定位到「你在创建时
// 把磁盘调小了」。
func TestCloneRaisesDiskToTemplateMinimum(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	path := "/tpl/min.qcow2"
	tpl := model.Template{
		NodeID: 1, Name: "tpl-min", Status: model.TemplateReady,
		CloneEnabled: true, DiskPath: &path, MinDiskGB: 50, CreatedBy: ptr(int64(7)),
	}
	db.Create(&tpl)

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "clone-min", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 10,
		TemplateID: tpl.ID, CloneMode: model.CloneFull,
	}, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	created := waitForVMNamed(t, db, "clone-min")
	if created.DiskGB != 50 {
		t.Errorf("磁盘 = %d GB, 期望被抬到模板最小值 50", created.DiskGB)
	}
}

func boolTag(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
