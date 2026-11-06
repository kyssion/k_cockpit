package template_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/template"
)

// TestListByKeywordOnSQLite 覆盖第 3 轮 DEF-R3-02：
// `GET /templates?keyword=` 在 SQLite 上 500。
//
// 根因：查询用的是 `name ILIKE ?`——PostgreSQL 专有语法。真机就是 SQLite，
// 于是"按关键字搜模板"从来没成功过一次（500），而**不带 keyword** 的列表
// 一切正常，很容易被当成偶发。
func TestListByKeywordOnSQLite(t *testing.T) {
	svc, db := newTestEnv(t, model.TemplateReady)
	for _, n := range []string{"ubuntu-2204", "Ubuntu-Server", "debian-12"} {
		row := model.Template{
			NodeID: 1, Name: n, Status: model.TemplateReady,
			DiskFormat: "qcow2", DiskSizeGB: 10,
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatalf("写入模板失败: %v", err)
		}
	}
	ctx := context.Background()
	v := authz.Viewer{UserID: 1, IsAdmin: true}

	all, err := svc.List(ctx, template.ListFilter{NodeID: 1, Viewer: v})
	if err != nil {
		t.Fatalf("不带 keyword 的列表失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("应返回 3 个模板，实际 %d", len(all))
	}

	// 关键字分支：不报 500，且大小写不敏感地只匹配到 2 个 ubuntu。
	got, err := svc.List(ctx, template.ListFilter{NodeID: 1, Keyword: "UBUNTU", Viewer: v})
	if err != nil {
		t.Fatalf("按关键字搜索失败（ILIKE 在 SQLite 上会 500）: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应匹配 2 个 ubuntu 模板，实际 %d", len(got))
	}
	for _, tv := range got {
		if !strings.Contains(strings.ToLower(tv.Name), "ubuntu") {
			t.Errorf("返回了不匹配的模板: %s", tv.Name)
		}
	}

	// 没有匹配时返回空列表而不是报错。
	empty, err := svc.List(ctx, template.ListFilter{NodeID: 1, Keyword: "no-such-tpl", Viewer: v})
	if err != nil {
		t.Fatalf("空结果不该报错: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("应返回空列表，实际 %d 条", len(empty))
	}
}
