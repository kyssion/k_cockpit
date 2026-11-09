package template_test

import (
	"context"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/template"
)

// TestUpdateRejectsUnknownVisibility 覆盖「可见性可以是任意字符串」。
//
// 实机现象：PATCH visibility=nonsense → 200 且落库，随后按租户视角的列表
// 过滤把这条模板算成"不满足条件"——它既不是已发布、也不是自己的，于是从
// 每个人的列表里消失，而没有任何一处报错。可见性决定「别人能不能看到这个
// 模板」，这类字段必须在落库前收敛到枚举。
func TestUpdateRejectsUnknownVisibility(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()
	tpl := readyTemplate(t, db, "tpl-vis")
	viewer := authz.Viewer{UserID: 7}

	for _, bad := range []string{"nonsense", "banana", "0", "priv ate", "privé"} {
		_, err := svc.Update(ctx, tpl.ID, template.UpdateRequest{Visibility: bad},
			viewer, "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("可见性 %q 应被拒绝", bad)
			continue
		}
		assertStatus(t, err, 422)
	}

	// 落库值必须仍是原值：被拒绝的请求不能顺手改掉其它字段。
	var stored model.Template
	if err := db.First(&stored, tpl.ID).Error; err != nil {
		t.Fatalf("读取模板失败: %v", err)
	}
	if stored.Visibility != model.TemplatePrivate {
		t.Errorf("被拒绝后可见性 = %q, 期望仍是 private", stored.Visibility)
	}

	// 两个合法值都要能过——枚举校验不能把正常功能挡死。
	for _, ok := range []string{model.TemplatePublic, model.TemplatePrivate} {
		view, err := svc.Update(ctx, tpl.ID, template.UpdateRequest{Visibility: ok},
			viewer, "alice", "10.0.0.1")
		if err != nil {
			t.Fatalf("合法可见性 %q 应被接受: %v", ok, err)
		}
		if view.Visibility != ok {
			t.Errorf("返回视图可见性 = %q, 期望 %q", view.Visibility, ok)
		}
	}

	// 大小写与首尾空白都归一化后落库：界面/脚本传 `PUBLIC`、或从别处复制
	// 粘贴带上空格都很常见，为此报错属于吹毛求疵；而"大小写不一致的值落进
	// 按等值字符串比较的可见性判定里"才是真正会让模板对所有人消失的那个方向。
	for _, raw := range []string{" public ", "PUBLIC", "Public"} {
		view, err := svc.Update(ctx, tpl.ID, template.UpdateRequest{Visibility: raw},
			viewer, "alice", "10.0.0.1")
		if err != nil {
			t.Fatalf("可见性 %q 应被接受并归一化: %v", raw, err)
		}
		if view.Visibility != model.TemplatePublic {
			t.Errorf("输入 %q 落库后 = %q, 期望归一化为 public", raw, view.Visibility)
		}
	}
}
