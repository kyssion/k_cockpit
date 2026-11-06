package template_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/compute/template"
)

// 本文件覆盖第 3 轮两条 P1：
//   - DEF-R3-06：软删同名模板仍在占位 → 预览说能导、实际导入必然失败；
//   - DEF-R3-01：导出包的 SHA256 在节点侧算好了，控制面投影里却没有。

// TestTemplateNameTakenIgnoresSoftDeleted 是 DEF-R3-06 的根因断言。
//
// 模板是软删除。旧索引不排除软删行，而预览（GORM 自动过滤软删行）看到的是
// "没有同名"——两处判定分叉，于是预览 can_import=true、落库必然撞约束。
func TestTemplateNameTakenIgnoresSoftDeleted(t *testing.T) {
	_, db := newTestEnv(t, model.TemplateReady)
	now := time.Now()

	// 一条已删除的同名模板。
	gone := model.Template{
		NodeID: 1, Name: "ubuntu-22", Status: model.TemplateReady,
		DiskFormat: "qcow2", DiskSizeGB: 10, DeletedAt: gorm.DeletedAt{Time: now, Valid: true},
	}
	if err := db.Create(&gone).Error; err != nil {
		t.Fatalf("写入软删模板失败: %v", err)
	}

	// 1) 唯一索引必须**允许**同名的在用模板（这就是部分唯一索引的作用）。
	fresh := model.Template{
		NodeID: 1, Name: "ubuntu-22", Status: model.TemplateReady,
		DiskFormat: "qcow2", DiskSizeGB: 10,
	}
	if err := db.Create(&fresh).Error; err != nil {
		t.Fatalf("软删行不该占住名字（DEF-R3-06 会复现）: %v", err)
	}

	// 2) 两条同名的在用模板必须仍然被拒（约束不能失效）。
	dup := model.Template{
		NodeID: 1, Name: "ubuntu-22", Status: model.TemplateReady,
		DiskFormat: "qcow2", DiskSizeGB: 10,
	}
	if err := db.Create(&dup).Error; err == nil {
		t.Fatal("两条在用的同名模板必须冲突")
	}
}

// TestTemplateIndexIsPartial 直接读库里的索引定义（根因形状）。
func TestTemplateIndexIsPartial(t *testing.T) {
	_, db := newTestEnv(t, model.TemplateReady)
	var sqlText string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='index' AND name=?", "uniq_template_node_name").
		Scan(&sqlText).Error; err != nil {
		t.Fatalf("读索引定义失败: %v", err)
	}
	if sqlText == "" {
		t.Fatal("测试库里没有 uniq_template_node_name")
	}
	lower := strings.ToLower(strings.ReplaceAll(sqlText, " ", ""))
	if !strings.Contains(lower, "deleted_atisnull") {
		t.Errorf("索引必须带 WHERE deleted_at IS NULL（否则软删行永久占位）: %s", sqlText)
	}
	if !strings.Contains(lower, "node_id") || !strings.Contains(lower, "name") {
		t.Errorf("索引列不对: %s", sqlText)
	}
}

// TestExportViewCarriesSHA256 覆盖 DEF-R3-01 的投影。
//
// 节点在打包时算好了摘要（TemplateExportInfo.SHA256），控制面以前直接丢掉，
// 于是接口里看不到它——用户无法在导入前核对包是否被改动过。
func TestExportViewCarriesSHA256(t *testing.T) {
	sum := strings.Repeat("a1b2", 16)
	view := template.ExportView{SHA256: sum, Filename: "pkg.tar.gz"}
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(blob), `"sha256"`) {
		t.Fatalf("导出视图必须透出 sha256 字段，实际: %s", blob)
	}
	if !strings.Contains(string(blob), sum) {
		t.Errorf("sha256 值没有出现在响应里: %s", blob)
	}
}
