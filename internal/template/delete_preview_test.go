package template_test

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

func adminViewer() authz.Viewer {
	return authz.Viewer{UserID: 1, IsAdmin: true}
}

func seedTemplate(t *testing.T, db *gorm.DB, id int64, name string, parentID *int64) {
	t.Helper()
	path := "/var/lib/k_cockpit/templates/" + name + ".qcow2"
	by := int64(1)
	tpl := model.Template{
		ID: id, Name: name, NodeID: 1,
		Status: model.TemplateReady, Visibility: model.TemplatePrivate,
		DiskPath: &path, ParentID: parentID, CreatedBy: &by,
	}
	if err := db.Create(&tpl).Error; err != nil {
		t.Fatalf("建模板失败: %v", err)
	}
}

func seedLinkedVM(t *testing.T, db *gorm.DB, templateID int64, name string) {
	t.Helper()
	owner := int64(1)
	vm := model.VM{
		Name: name, NodeID: 1, TemplateID: &templateID,
		CloneMode: model.CloneLinked, Present: true, OwnerID: &owner,
		Status: model.VMStatusStopped,
	}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("建虚拟机失败: %v", err)
	}
}

// TestDeletePreviewListsNamesNotJustCount 覆盖预览最要紧的一点。
//
// 「仍有 3 台链式克隆依赖它」只告诉用户有麻烦，而没说找谁——他要拿这个去
// 处理，就必须知道是哪几台。计数与名单都能给，而名单才是能用的那个。
func TestDeletePreviewListsNamesNotJustCount(t *testing.T) {
	tsvc, db := newTestEnv(t, "shutdown")
	ctx := context.Background()
	seedTemplate(t, db, 10, "base-image", nil)
	seedLinkedVM(t, db, 10, "web-01")
	seedLinkedVM(t, db, 10, "web-02")

	view, err := tsvc.DeletePreview(ctx, 10, adminViewer())
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if view.CanDelete {
		t.Fatal("有依赖时不该显示「可以删除」")
	}
	if len(view.Blockers) != 1 {
		t.Fatalf("应有 1 条依赖，实际 %d", len(view.Blockers))
	}
	b := view.Blockers[0]
	if b.Count != 2 {
		t.Errorf("计数 = %d, 期望 2", b.Count)
	}
	if len(b.Names) != 2 {
		t.Fatalf("应列出 2 个名字，实际 %v", b.Names)
	}
	got := map[string]bool{b.Names[0]: true, b.Names[1]: true}
	if !got["web-01"] || !got["web-02"] {
		t.Errorf("名字不对: %v", b.Names)
	}
	// 必须说清怎么解决，而不只是「不行」。
	if b.Fix == "" {
		t.Error("应给出解决方式")
	}
	if view.LinkedVMCount != 2 {
		t.Errorf("LinkedVMCount = %d, 期望 2", view.LinkedVMCount)
	}
}

// TestDeletePreviewReportsAllBlockersAtOnce 覆盖「一次给全」。
//
// 撞到第一个就返回的话，用户解决了链式克隆再点一次，又会撞上派生模板——
// 两次往返换来的信息本可以一次给全。而预览存在的意义正是把待办一次说清。
func TestDeletePreviewReportsAllBlockersAtOnce(t *testing.T) {
	tsvc, db := newTestEnv(t, "shutdown")
	ctx := context.Background()
	seedTemplate(t, db, 10, "base", nil)
	seedLinkedVM(t, db, 10, "vm-a")
	parent := int64(10)
	seedTemplate(t, db, 11, "derived", &parent)

	view, err := tsvc.DeletePreview(ctx, 10, adminViewer())
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if view.CanDelete {
		t.Fatal("有依赖时不该显示「可以删除」")
	}
	if len(view.Blockers) != 2 {
		t.Fatalf("两种依赖应**一次全部**报告，实际 %d 条: %+v", len(view.Blockers), view.Blockers)
	}
	kinds := map[string]bool{}
	for _, b := range view.Blockers {
		kinds[b.Kind] = true
	}
	if !kinds["linked_vm"] || !kinds["child_template"] {
		t.Errorf("应同时报告两类依赖，实际 %v", kinds)
	}
}

// TestDeletePreviewAllowsWhenNoDependency 覆盖「没有依赖时如实说可以删」。
//
// 并且要给出**会释放什么**（磁盘路径）：用户在这一步要确认的不只是"能不能删"，
// 还有"删掉的是什么"。
func TestDeletePreviewAllowsWhenNoDependency(t *testing.T) {
	tsvc, db := newTestEnv(t, "shutdown")
	ctx := context.Background()
	seedTemplate(t, db, 10, "lonely", nil)

	view, err := tsvc.DeletePreview(ctx, 10, adminViewer())
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if !view.CanDelete {
		t.Fatalf("没有依赖时应显示可以删除，实际 blockers=%+v", view.Blockers)
	}
	if len(view.Blockers) != 0 {
		t.Errorf("不该有 blocker，实际 %+v", view.Blockers)
	}
	if view.DiskPath == "" {
		t.Error("应给出删除后会释放的磁盘路径——用户要确认的是「删掉的是什么」")
	}
}

// TestDeletePreviewMatchesDelete 覆盖预览与实际删除的**一致性**。
//
// 两处各写一遍判定迟早分叉，而分叉的表现是「预览说可以删、点下去却报冲突」
// ——那会让用户以为预览在骗他，而这恰恰是预览存在意义的反面。
func TestDeletePreviewMatchesDelete(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		deps    bool
		wantErr bool
	}{
		{"无依赖：预览可删、实际可删", false, false},
		{"有依赖：预览不可删、实际拒绝", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tsvc, db := newTestEnv(t, "shutdown")
			seedTemplate(t, db, 10, "t", nil)
			if tc.deps {
				seedLinkedVM(t, db, 10, "vm-x")
			}

			view, err := tsvc.DeletePreview(ctx, 10, adminViewer())
			if err != nil {
				t.Fatalf("预览失败: %v", err)
			}

			_, delErr := tsvc.Delete(ctx, 10, adminViewer(), "root", "")
			if (delErr != nil) != tc.wantErr {
				t.Fatalf("删除结果与预期不符: err=%v", delErr)
			}
			// 核心断言：预览说的与删除做的一致。
			if view.CanDelete == tc.wantErr {
				t.Errorf("预览说 can_delete=%v，而实际删除%s——两者必须一致",
					view.CanDelete, map[bool]string{true: "被拒绝", false: "成功"}[tc.wantErr])
			}
		})
	}
}

// TestDeletePreviewByNonOwnerIsHidden 覆盖越权访问的行为。
//
// 别人的未发布模板应当**报不存在**而不是报无权限——后者会泄露"这个 id 上
// 有东西"。这与本包 load 的既有约定一致。
func TestDeletePreviewByNonOwnerIsHidden(t *testing.T) {
	tsvc, db := newTestEnv(t, "shutdown")
	ctx := context.Background()
	seedTemplate(t, db, 10, "private", nil)

	other := authz.Viewer{UserID: 99}
	if _, err := tsvc.DeletePreview(ctx, 10, other); err == nil {
		t.Fatal("别人的未发布模板应当不可见")
	}
}
