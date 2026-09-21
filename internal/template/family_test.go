package template_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/template"
)

// seedFamily 造一条三代的派生链：v1 → v2 → v3。
func seedFamily(t *testing.T, db *gorm.DB) (v1, v2, v3 int64) {
	t.Helper()
	root := model.Template{
		ID: 1, NodeID: 1, Name: "base-v1", Status: model.TemplateReady, Version: 1,
		CreatedBy: ptr(int64(1)), CloneEnabled: true,
	}
	if err := db.Create(&root).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}
	second := model.Template{
		ID: 2, NodeID: 1, Name: "base-v2", Status: model.TemplateReady, Version: 2,
		ParentID: ptr(int64(1)), FamilyID: ptr(int64(1)),
		CreatedBy: ptr(int64(1)), CloneEnabled: true,
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}
	third := model.Template{
		ID: 3, NodeID: 1, Name: "base-v3", Status: model.TemplateReady, Version: 3,
		ParentID: ptr(int64(2)), FamilyID: ptr(int64(1)),
		CreatedBy: ptr(int64(1)), CloneEnabled: true,
	}
	if err := db.Create(&third).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}
	return 1, 2, 3
}

// TestFamilyReturnsWholeChain 覆盖「族」的口径：同一条派生链上的全部版本。
//
// 只认直接子级是不够的：v3 并不以 v1 为父，但它同样是这一族的一员，删掉
// v1 时它一样会受影响。
func TestFamilyReturnsWholeChain(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	seedFamily(t, db)
	ctx := context.Background()

	items, err := svc.Family(ctx, 1, adminViewer())
	if err != nil {
		t.Fatalf("查询模板族失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("族内模板数 = %d, 期望 3（含间接派生的 v3）", len(items))
	}
	// 按版本排序：版本号才是「第几代」。
	if items[0].Version != 1 || items[2].Version != 3 {
		t.Errorf("排序 = %d/%d/%d, 期望按版本升序", items[0].Version, items[1].Version, items[2].Version)
	}
	// 从中间一代查询也应得到同一族。
	middle, err := svc.Family(ctx, 2, adminViewer())
	if err != nil {
		t.Fatalf("查询模板族失败: %v", err)
	}
	if len(middle) != 3 {
		t.Errorf("从 v2 查询族 = %d, 期望 3", len(middle))
	}
}

// TestDeleteBlockersCoverIndirectChildren 覆盖子树统计。
//
// "只报直接子级"的表现是：用户处理完 v2 再点一次删除，才被告知还有 v3——
// 两次往返换来的信息本可以一次给全。
func TestDeleteBlockersCoverIndirectChildren(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	seedFamily(t, db)
	ctx := context.Background()

	view, err := svc.DeletePreview(ctx, 1, adminViewer())
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if view.CanDelete {
		t.Fatal("有派生模板却说可以删除")
	}
	if view.ChildCount != 2 {
		t.Errorf("派生模板数 = %d, 期望 2（v2 与间接派生的 v3）", view.ChildCount)
	}
	// 策略由服务端下发，前端不猜。
	if len(view.Strategies) == 0 {
		t.Error("未提供可用的删除策略")
	}
}

// TestDeleteWithoutStrategyIsRejected 覆盖「必须显式选策略」。
//
// 删掉中间一代有两种合理做法（级联、提升），而它们的结果完全不同——
// 服务端替用户选一个等于把这个决定藏起来。
func TestDeleteWithoutStrategyIsRejected(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	seedFamily(t, db)
	ctx := context.Background()

	_, err := svc.Delete(ctx, 1, template.DeleteRequest{}, adminViewer(), "root", "")
	if err == nil {
		t.Fatal("未指定策略却删除成功了")
	}
	if !strings.Contains(err.Error(), "级联") && !strings.Contains(err.Error(), "提升") {
		t.Errorf("错误应说明可选的做法: %v", err)
	}
}

// TestDeletePromoteKeepsDescendants 覆盖提升策略：只删这一代，下游改挂到
// 它的父级上。
func TestDeletePromoteKeepsDescendants(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	seedFamily(t, db)
	ctx := context.Background()

	if _, err := svc.Delete(ctx, 2, template.DeleteRequest{Strategy: template.DeleteStrategyPromote},
		adminViewer(), "root", ""); err != nil {
		t.Fatalf("提升删除失败: %v", err)
	}

	// v3 不再指向被删掉的 v2，而是改挂到 v1。
	var v3 model.Template
	if err := db.Where("id = ?", 3).First(&v3).Error; err != nil {
		t.Fatalf("读取 v3 失败: %v", err)
	}
	if v3.ParentID == nil || *v3.ParentID != 1 {
		t.Errorf("v3 的父级 = %v, 期望提升到 1", v3.ParentID)
	}
	// 族不变：它们仍然同源。
	if v3.FamilyID == nil || *v3.FamilyID != 1 {
		t.Errorf("v3 的族 = %v, 期望保持 1", v3.FamilyID)
	}
}

// TestPrepareDerivedAssignsFamilyAndVersion 覆盖派生制备时的族与版本号。
//
// 版本按**族内最大值 + 1** 而不是父版本 + 1：同一个父下派生两次应得到
// v2 与 v3，按父版本算则两者都叫 v2。
func TestPrepareDerivedAssignsFamilyAndVersion(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	seedFamily(t, db)
	ctx := context.Background()
	vmRow := seedVM(t, db, "vm-derived", 7)

	tk, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "base-v4", ParentID: ptr(int64(3)),
	}, adminViewer(), "root", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, func() bool {
		var n int64
		db.Model(&model.Template{}).Where("name = ?", "base-v4").Count(&n)
		return n == 1
	})

	var created model.Template
	if err := db.Where("name = ?", "base-v4").First(&created).Error; err != nil {
		t.Fatalf("读取模板失败: %v", err)
	}
	if created.ParentID == nil || *created.ParentID != 3 {
		t.Errorf("父级 = %v, 期望 3", created.ParentID)
	}
	if created.FamilyID == nil || *created.FamilyID != 1 {
		t.Errorf("族 = %v, 期望 1（继承根的族）", created.FamilyID)
	}
	if created.Version != 4 {
		t.Errorf("版本 = %d, 期望 4（族内最大值 + 1）", created.Version)
	}
	// 默认硬件取自源虚拟机：否则克隆出来的是另一台机器。
	if created.DefaultDiskBus == nil || *created.DefaultDiskBus != vmRow.DiskBus {
		t.Errorf("默认磁盘驱动 = %v, 期望源虚拟机的 %q", created.DefaultDiskBus, vmRow.DiskBus)
	}
	_ = tk
}

// TestPrepareRejectsParentOnAnotherNode 覆盖「派生必须同节点」。
//
// 模板盘就在那个节点的存储池里，跨节点的"新版本"既无法复用 backing 链，
// 也无从保证内容真的来自那个父模板。
func TestPrepareRejectsParentOnAnotherNode(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	seedFamily(t, db)
	ctx := context.Background()

	remote := model.VM{
		NodeID: 9, Name: "vm-remote", Status: model.VMStatusStopped,
		VCPU: 1, MemoryMB: 1024, DiskGB: 10, Present: true, OwnerID: ptr(int64(7)),
	}
	if err := db.Create(&remote).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	_, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: remote.ID, Name: "bad-derived", ParentID: ptr(int64(1)),
	}, adminViewer(), "root", "10.0.0.1")
	if err == nil {
		t.Fatal("跨节点派生未被拒绝")
	}
	if !strings.Contains(err.Error(), "节点") {
		t.Errorf("错误未指出节点不匹配: %v", err)
	}
}

func ptr(v int64) *int64 { return &v }
