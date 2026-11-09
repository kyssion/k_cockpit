package template_test

import (
	"context"
	"fmt"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/compute/template"
)

// TestPromoteFromRootStartsAtTwo 覆盖「派生第一代与根重号」。
//
// 实机现象：根模板 v1（未派生过，family_id 为空）→ 从它派生新版本，新模板
// 的 version 仍是 1，家族接口返回 versions=[1,1]；此后每一代都整体偏小 1。
//
// 根因是算版本号时只统计 `family_id = ?`，而**根模板自身的 family_id 是
// NULL**——族的范围必须与 Family 接口一致（`id = ? OR family_id = ?`）。
// 这条用例从"还没有任何子级的根"连续提升三次，要求版本号严格递增。
func TestPromoteFromRootStartsAtTwo(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()

	root := readyTemplate(t, db, "chain-v1")
	vmRow := seedVM(t, db, "vm-chain", 7)

	for _, want := range []int{2, 3, 4} {
		name := fmt.Sprintf("chain-v%d", want)
		if _, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
			VMID: vmRow.ID, Name: name, ParentID: ptr(root.ID),
		}, adminViewer(), "root", "10.0.0.1"); err != nil {
			t.Fatalf("提升到 v%d 受理失败: %v", want, err)
		}
		waitFor(t, func() bool {
			var n int64
			db.Model(&model.Template{}).Where("name = ?", name).Count(&n)
			return n == 1
		})

		var created model.Template
		if err := db.Where("name = ?", name).First(&created).Error; err != nil {
			t.Fatalf("读取模板失败: %v", err)
		}
		if created.Version != want {
			t.Fatalf("从根派生的第 %d 代版本 = %d, 期望 %d（族内最大值 + 1）",
				want-1, created.Version, want)
		}
	}

	// 家族视图里版本号必须唯一且升序：界面靠它区分"第几代"，
	// 导出包名 `<name>-v<version>` 也按它取数，重号会导致同名覆盖。
	views, err := svc.Family(ctx, root.ID, adminViewer())
	if err != nil {
		t.Fatalf("查询家族失败: %v", err)
	}
	if len(views) != 4 {
		t.Fatalf("家族成员数 = %d, 期望 4", len(views))
	}
	seen := map[int]bool{}
	for _, v := range views {
		if seen[v.Version] {
			t.Errorf("家族内版本号重复: %d（versions=%v）", v.Version, versionsOf(views))
		}
		seen[v.Version] = true
	}
	for i, want := range []int{1, 2, 3, 4} {
		if views[i].Version != want {
			t.Errorf("第 %d 个成员的版本 = %d, 期望 %d", i, views[i].Version, want)
		}
	}
}

// versionsOf 取出家族成员的版本号，供失败信息使用。
func versionsOf(views []template.View) []int {
	out := make([]int, 0, len(views))
	for _, v := range views {
		out = append(out, v.Version)
	}
	return out
}
