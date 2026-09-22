package importer_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/importer"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

func newTestEnv(t *testing.T) (*importer.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "import.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.ImageImport{}, &model.Template{}, &model.Node{},
		&model.Task{}, &model.TaskStage{}, &model.AuditLog{},
		&model.StorageFile{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 1,
		PollInterval:  20 * time.Millisecond,
	})
	queue.Register(importer.NewImportExecutor(db, client))

	// 预置测试用到的源文件（G-34 起导入校验文件已上传）：文件名与各用例
	// 使用的保持一致，category=disk、已上传（uploaded_at 非空）。
	now := time.Now()
	for _, name := range []string{
		"ubuntu.qcow2", "fast.qcow2", "disk.qcow2", "old.vmdk", "appliance.ova",
	} {
		if err := db.Create(&model.StorageFile{
			NodeID: 1, UserID: iptr(7), Category: model.FileCategoryDisk,
			RelPath:  "disk/" + name,
			Filename: name, SizeBytes: 1 << 30, UploadedAt: &now,
		}).Error; err != nil {
			t.Fatalf("预置源文件 %s 失败: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	return importer.NewService(db, queue, client, recorder, nil), db
}

// TestFormatInferenceFromFilename 覆盖按扩展名推断格式。
//
// 由**后端**推断而不是信前端传来的值：扩展名是用户唯一会认真看的东西，
// 而前端传的 formats 字段可能因为版本旧或请求被改而失真。
func TestFormatInferenceFromFilename(t *testing.T) {
	cases := []struct {
		filename string
		want     string
		ok       bool
	}{
		{"ubuntu.qcow2", "qcow2", true},
		{"disk.RAW", "raw", true},
		{"legacy.vmdk", "vmdk", true},
		{"win2019.vhdx", "vhdx", true},
		{"appliance.ova", "ova", true},
		{"noext", "", false},
		{"archive.tar.gz", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := importer.FormatFromFilename(tc.filename)
		if ok != tc.ok {
			t.Errorf("%q: ok = %v, 期望 %v", tc.filename, ok, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: 格式 = %q, 期望 %q", tc.filename, got, tc.want)
		}
	}
}

// TestCreateRejectsMismatchedExtension 覆盖一条刻意的严格。
//
// 扩展名与声明的格式不一致时**报错而不是猜**：按其中任一个去执行都可能
// 做错事（按错误的格式转换会产出一块用不了的盘），而报出来让人查成本最低。
func TestCreateRejectsMismatchedExtension(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "tpl-x",
		SourceFilename: "disk.qcow2", SourceFormat: model.ImportVMDK,
		DiskGB: 20,
	}, authz.Viewer{UserID: 7}, "alice", "")
	assertStatus(t, err, 400)
}

func TestCreateRejectsUnsupportedFormat(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "tpl-y",
		SourceFilename: "disk.iso", SourceFormat: "iso", DiskGB: 20,
	}, authz.Viewer{UserID: 7}, "alice", "")
	assertStatus(t, err, 400)
}

// TestImportProducesTemplate 覆盖「产物是模板」这条设计选择。
//
// 导入一份别人的镜像之后，用户接下来多半是「用它开几台机」，而不是「就要
// 这一台」。产出模板把人带到已有的克隆流程上，也顺便获得了模板那套可见性
// 与依赖管理。
func TestImportProducesTemplate(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	tk, err := svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "ubuntu-import",
		SourceFilename: "ubuntu.qcow2", SourceFormat: model.ImportQCOW2,
		SourceSizeBytes: 3 << 30,
		VCPU:            4, MemoryMB: 8192, DiskGB: 40,
		OSType: "linux",
	}, viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, func() bool {
		var n int64
		db.Model(&model.Template{}).Where("name = ?", "ubuntu-import").Count(&n)
		return n == 1
	})

	var tpl model.Template
	db.Where("name = ?", "ubuntu-import").First(&tpl)
	if tpl.DefaultCPU != 4 || tpl.DefaultMemoryMB != 8192 {
		t.Errorf("模板默认规格 = %d 核 / %d MB，期望 4 / 8192",
			tpl.DefaultCPU, tpl.DefaultMemoryMB)
	}
	if !tpl.CloneEnabled {
		t.Error("导入产出的模板应可直接克隆")
	}

	// 导入记录上要能查到产出的模板——界面据此给出「去克隆一台」的入口。
	// 没有它的话，用户导入完会停在那里不知道下一步做什么。
	view, err := svc.Get(ctx, 1, viewer)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view.Status != model.ImportSuccess {
		t.Errorf("状态 = %q, 期望 success", view.Status)
	}
	if view.TemplateID == nil || *view.TemplateID != tpl.ID {
		t.Errorf("未记录产出模板: %v", view.TemplateID)
	}
	_ = tk
}

// TestImportRecordExistsWhileRunning 覆盖受理即建记录。
//
// 导入是长任务（格式转换可能处理几十 GB），用户需要在那段时间里看到
// 「有一个导入在进行」；否则界面上什么都没有，他会以为没点上。
func TestImportRecordExistsWhileRunning(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	if _, err := svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "fast-import",
		SourceFilename: "fast.qcow2", SourceFormat: model.ImportQCOW2,
		DiskGB: 20,
	}, viewer, "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	items, err := svc.List(ctx, viewer, 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("导入记录数 = %d, 期望 1（受理时即创建）", len(items))
	}
}

// TestParseBareImageSaysUserSource 覆盖裸镜像的预览。
//
// 裸镜像不含机器描述，因此预览里的值是**用户将要使用的值**，来源标为 user。
// 这与 OVA 的 ovf 来源不同——用户需要知道「这个 4 核是我自己选的，还是从
// 包里读出来的」，两者对错误的含义完全不同。
func TestParseBareImageSaysUserSource(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	preview, err := svc.Parse(ctx, importer.ParseRequest{
		NodeID: 1, SourceFilename: "disk.qcow2", SourceFormat: model.ImportQCOW2,
	}, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if preview.Sources["vcpu"] != "user" {
		t.Errorf("裸镜像的 vcpu 来源 = %q, 期望 user", preview.Sources["vcpu"])
	}

	// vmdk 要提示会自动转换——用户在导入前就该知道这件事。
	vmdk, err := svc.Parse(ctx, importer.ParseRequest{
		NodeID: 1, SourceFilename: "old.vmdk", SourceFormat: model.ImportVMDK,
	}, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	found := false
	for _, n := range vmdk.Notes {
		if len(n) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("非 qcow2 格式应给出「会自动转换」的提示")
	}
}

// TestParseOvaReturnsOvfValues 覆盖 OVA 的预览来自包内描述。
func TestParseOvaReturnsOvfValues(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	preview, err := svc.Parse(ctx, importer.ParseRequest{
		NodeID: 1, SourceFilename: "appliance.ova", SourceFormat: model.ImportOVA,
	}, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if preview.Sources["vcpu"] != "ovf" {
		t.Errorf("OVA 的 vcpu 来源 = %q, 期望 ovf", preview.Sources["vcpu"])
	}
	// mock 给的是 4 核 / 8 GB——刻意非默认，好让「用解析值覆盖默认值」
	// 那段逻辑真的走到。
	if preview.VCPU != 4 || preview.MemoryMB != 8192 {
		t.Errorf("解析出的配置 = %d 核 / %d MB，期望 4 / 8192",
			preview.VCPU, preview.MemoryMB)
	}
}

// TestVisibilityScopesToOwner 覆盖可见性。
func TestVisibilityScopesToOwner(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	mine, theirs := int64(7), int64(8)
	db.Create(&model.ImageImport{
		NodeID: 1, Name: "mine", SourceFilename: "a.qcow2",
		SourceFormat: model.ImportQCOW2, CreatedBy: &mine,
	})
	db.Create(&model.ImageImport{
		NodeID: 1, Name: "theirs", SourceFilename: "b.qcow2",
		SourceFormat: model.ImportQCOW2, CreatedBy: &theirs,
	})

	items, err := svc.List(ctx, authz.Viewer{UserID: 7}, 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 1 || items[0].Name != "mine" {
		t.Errorf("普通用户应只看到自己的导入记录, 实际 %d 条", len(items))
	}

	all, err := svc.List(ctx, authz.Viewer{UserID: 99, IsAdmin: true}, 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("管理员应看到全部, 实际 %d 条", len(all))
	}

	// 别人的记录用 404 而非 403：403 会确认「这个 ID 存在」。
	_, err = svc.Get(ctx, 2, authz.Viewer{UserID: 7})
	assertStatus(t, err, 404)
}

func TestDeleteRejectsRunningImport(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	running := model.ImageImport{
		NodeID: 1, Name: "running", SourceFilename: "r.qcow2",
		SourceFormat: model.ImportQCOW2, Status: model.ImportRunning, CreatedBy: &u,
	}
	db.Create(&running)

	err := svc.Delete(ctx, running.ID, authz.Viewer{UserID: 7}, "alice", "")
	assertStatus(t, err, 409)
}

func TestDeleteKeepsProducedTemplate(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)
	viewer := authz.Viewer{UserID: 7}

	tpl := model.Template{
		NodeID: 1, Name: "keep-me", Status: model.TemplateReady,
		DiskSizeGB: 20, CreatedBy: &u,
	}
	db.Create(&tpl)
	row := model.ImageImport{
		NodeID: 1, Name: "keep-me", SourceFilename: "k.qcow2",
		SourceFormat: model.ImportQCOW2, Status: model.ImportSuccess,
		CreatedBy: &u, TemplateID: &tpl.ID,
	}
	db.Create(&row)

	// 删记录时**不能顺手删模板**：模板可能已经被克隆成多台虚拟机，
	// 而那些虚拟机在磁盘上依赖的可能正是这份模板。
	if err := svc.Delete(ctx, row.ID, viewer, "alice", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	var count int64
	db.Model(&model.Template{}).Where("id = ?", tpl.ID).Count(&count)
	if count != 1 {
		t.Error("删除导入记录时把产出的模板也删了——依赖它的虚拟机可能仍在运行")
	}
}

// --- 辅助 ---

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待导入完成超时")
}

// iptr 是 int64 指针的简写（本包测试没有现成的同名辅助）。
func iptr(v int64) *int64 { return &v }

// TestCreateRejectsUnuploadedSource 覆盖源文件存在性校验（G-34）。
//
// 导入按文件名定位文件：没上传过的名字如果放过去，会建出一条注定失败的
// 任务，用户要等任务跑到一半才知道错。校验在受理时拦住它，并给出
// 「去哪上传」的可执行指引。他人上传的文件按不存在处理（404），
// 不确认「它存在」。
func TestCreateRejectsUnuploadedSource(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	_, err := svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "ghost-import",
		SourceFilename: "never-uploaded.qcow2", SourceFormat: model.ImportQCOW2,
		DiskGB: 20,
	}, viewer, "alice", "")
	assertStatus(t, err, 422)

	var apiErr *api.Error
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Message, "上传") {
		t.Fatalf("错误文案未指向上传: %v", err)
	}

	// 他人（用户 8）上传的同名文件：普通用户按不存在处理。
	foreign := int64(8)
	now := time.Now()
	if err := db.Create(&model.StorageFile{
		NodeID: 1, UserID: &foreign, Category: model.FileCategoryDisk,
		RelPath: "disk/foreign.qcow2", Filename: "foreign.qcow2",
		SizeBytes: 1 << 30, UploadedAt: &now,
	}).Error; err != nil {
		t.Fatalf("预置他人文件失败: %v", err)
	}
	_, err = svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "steal-import",
		SourceFilename: "foreign.qcow2", SourceFormat: model.ImportQCOW2,
		DiskGB: 20,
	}, viewer, "alice", "")
	assertStatus(t, err, 404)

	// 管理员可以用任何人的文件。
	_, err = svc.Create(ctx, importer.CreateRequest{
		NodeID: 1, Name: "admin-import",
		SourceFilename: "foreign.qcow2", SourceFormat: model.ImportQCOW2,
		DiskGB: 20,
	}, authz.Viewer{UserID: 1, IsAdmin: true}, "admin", "")
	if err != nil {
		t.Fatalf("管理员导入他人文件被误拒: %v", err)
	}
}
