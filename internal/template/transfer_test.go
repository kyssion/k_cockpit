package template_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/template"
)

// seedStorageFile 在「我的存储」里放一个模板包。
func seedStorageFile(t *testing.T, db *gorm.DB, nodeID, userID int64, relPath, category string) int64 {
	t.Helper()
	now := time.Now()
	row := model.StorageFile{
		NodeID: nodeID, UserID: &userID, RelPath: relPath,
		Category: category, Filename: relPath, SizeBytes: 1 << 20,
		UploadedAt: &now,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建文件失败: %v", err)
	}
	return row.ID
}

// testEnvForTransfer 准备带导出/导入执行器的测试环境。
func testEnvForTransfer(t *testing.T) (*template.Service, *gorm.DB) {
	t.Helper()
	svc, db := newTestEnv(t, model.VMStatusStopped)
	client := &statusClient{MockClient: agent.NewMockClient(), status: model.VMStatusStopped}
	_ = client
	return svc, db
}

// TestExportRejectsTemplateNotReady 覆盖「只有就绪的模板能导出」。
//
// 制备中的模板盘还在被写入，打包它得到的包在另一台机器上导入后，
// 会是一台开机就要 fsck 的机器——而那时没人会想到是导出那一刻的问题。
func TestExportRejectsTemplateNotReady(t *testing.T) {
	svc, db := testEnvForTransfer(t)
	ctx := context.Background()

	tpl := model.Template{
		NodeID: 1, Name: "not-ready", Status: model.TemplatePreparing,
		CreatedBy: ptr(int64(1)),
	}
	if err := db.Create(&tpl).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}

	_, err := svc.Export(ctx, tpl.ID, authz.Viewer{UserID: 1, IsAdmin: true}, "root", "")
	if err == nil {
		t.Fatal("未就绪的模板也受理了导出")
	}
	if !strings.Contains(err.Error(), "就绪") {
		t.Errorf("错误应说明模板未就绪: %v", err)
	}
}

// TestImportPreviewRejectsWrongCategory 覆盖「类别必须是模板包」。
//
// 一个 .qcow2 被当成模板包解，节点会在解包那一步失败，而那时用户已经
// 等了几分钟——这类冲突在受理前就应该被看见。
func TestImportPreviewRejectsWrongCategory(t *testing.T) {
	svc, db := testEnvForTransfer(t)
	ctx := context.Background()

	id := seedStorageFile(t, db, 1, 1, "some.img", model.FileCategoryDisk)

	_, err := svc.ImportPreview(ctx, template.ImportRequest{FileID: id},
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err == nil {
		t.Fatal("非模板包也通过了预览")
	}
	if !strings.Contains(err.Error(), "模板包") {
		t.Errorf("错误应说明类别不对: %v", err)
	}
}

// TestImportPreviewShowsManifest 覆盖预览的内容。
//
// 先验后做是这里唯一合理的顺序：包是别人给的，控制面必须在受理前把
// "将导入成什么"显示给用户。
func TestImportPreviewShowsManifest(t *testing.T) {
	svc, db := testEnvForTransfer(t)
	ctx := context.Background()

	id := seedStorageFile(t, db, 1, 1, "package/base-v2.tar.gz", model.FileCategoryTemplatePackage)

	view, err := svc.ImportPreview(ctx, template.ImportRequest{FileID: id},
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if view.Manifest.Name == "" || view.Manifest.DiskFormat == "" {
		t.Errorf("清单不完整: %+v", view.Manifest)
	}
	if !view.CanImport {
		t.Errorf("应当可以导入: %s", view.Reason)
	}
}

// TestImportPreviewRejectsDigestMismatch 覆盖摘要不符。
//
// 这条失败路径必须可达：界面要能渲染出"包内容与清单不符"，
// 否则用户会在导入失败时看到一句空话。
func TestImportPreviewRejectsDigestMismatch(t *testing.T) {
	svc, db := testEnvForTransfer(t)
	ctx := context.Background()

	id := seedStorageFile(t, db, 1, 1, "package/corrupt.tar.gz", model.FileCategoryTemplatePackage)

	view, err := svc.ImportPreview(ctx, template.ImportRequest{FileID: id},
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if view.CanImport {
		t.Fatal("摘要不符却说可以导入")
	}
	if !strings.Contains(view.Reason, "摘要") {
		t.Errorf("未说明摘要不符: %s", view.Reason)
	}
}

// TestImportRejectsDuplicateName 覆盖同名冲突。
//
// 名字撞了是导入最常见的失败，而它**在导入之前**就能判定——放到执行阶段
// 才报错，用户已经等完了整个解包过程。
func TestImportRejectsDuplicateName(t *testing.T) {
	svc, db := testEnvForTransfer(t)
	ctx := context.Background()

	// mock 用包名推导模板名：base.tar.gz → base。
	id := seedStorageFile(t, db, 1, 1, "package/base.tar.gz", model.FileCategoryTemplatePackage)
	existing := model.Template{
		NodeID: 1, Name: "base", Status: model.TemplateReady,
		CreatedBy: ptr(int64(1)),
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}

	view, err := svc.ImportPreview(ctx, template.ImportRequest{FileID: id},
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if view.CanImport {
		t.Fatal("同名却说可以导入")
	}
	if !strings.Contains(view.Reason, "同名") {
		t.Errorf("未说明是重名: %s", view.Reason)
	}

	if _, err := svc.Import(ctx, template.ImportRequest{FileID: id},
		authz.Viewer{UserID: 1, IsAdmin: true}, "root", ""); err == nil {
		t.Fatal("同名却受理了导入")
	}
}

// TestListExportsFiltersByOwner 覆盖归属过滤。
//
// 导出包里是模板盘——私有模板不该因为一次导出就变成别人可见的东西。
func TestListExportsFiltersByOwner(t *testing.T) {
	svc, db := testEnvForTransfer(t)
	ctx := context.Background()

	mine := model.TemplateExport{
		NodeID: 1, TemplateID: 1, TemplateName: "mine",
		RelPath: "packages/mine.tar.gz", Filename: "mine.tar.gz",
		Status: model.TemplateExportSuccess, CreatedBy: ptr(int64(7)),
	}
	other := model.TemplateExport{
		NodeID: 1, TemplateID: 2, TemplateName: "other",
		RelPath: "packages/other.tar.gz", Filename: "other.tar.gz",
		Status: model.TemplateExportSuccess, CreatedBy: ptr(int64(9)),
	}
	db.Create(&mine)
	db.Create(&other)

	items, err := svc.ListExports(ctx, 1, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 1 || items[0].TemplateName != "mine" {
		t.Errorf("租户看到的导出 = %+v, 期望只看到自己的一条", items)
	}

	items, err = svc.ListExports(ctx, 1, authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("管理员看到的导出数 = %d, 期望 2", len(items))
	}
}
