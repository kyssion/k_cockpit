package diagnostics_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/diagnostics"
	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
	"k_cockpit/internal/settings"
)

func newEnv(t *testing.T) (*gorm.DB, *diagnostics.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "diag.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SystemSetting{}, &model.Node{}, &model.VM{}, &model.Task{},
		&model.AuditLog{}, &model.SchedulerEvent{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	setSvc := settings.NewService(db, audit.NewRecorder(db))
	return db, diagnostics.NewService(db, setSvc, scheduler.NewRegistry(), audit.NewRecorder(db))
}

func adminUser() diagnostics.Viewer {
	return diagnostics.Viewer{UserID: 1, IsAdmin: true, Username: "root"}
}

// readZip 把包解开成「文件名 → 内容」。
func readZip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("生成的不是合法 zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", f.Name, err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

// TestBundleContainsManifest 覆盖包的基本形状。
func TestBundleContainsManifest(t *testing.T) {
	_, svc := newEnv(t)

	b, err := svc.Export(context.Background(), nil, adminUser(), "root", "10.0.0.1")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if b.Filename == "" || !strings.HasSuffix(b.Filename, ".zip") {
		t.Errorf("文件名应带时刻与 .zip，实际 %q", b.Filename)
	}

	files := readZip(t, b.Data)
	if _, ok := files["MANIFEST.json"]; !ok {
		t.Fatal("包中应有 MANIFEST——它写明生成时刻与哪些内容被截断")
	}
	if !strings.Contains(files["MANIFEST.json"], "generated_at") {
		t.Error("MANIFEST 应含生成时刻：包里的数据是某一瞬间的，分析的人需要知道是哪一刻")
	}
	if _, ok := files["config/settings.json"]; !ok {
		t.Error("默认应包含配置分类")
	}
	if _, ok := files["runtime/overview.json"]; !ok {
		t.Error("默认应包含运行时状态分类")
	}
}

// TestSecretsNeverAppear 覆盖这块最要紧的一条。
//
// 排障包的用途就是发给别人。任何依赖用户"记得先把密码删掉"的安排都会失败
// ——他会直接发出去。因此敏感字段在生成的那一刻就必须被打码。
func TestSecretsNeverAppear(t *testing.T) {
	db, svc := newEnv(t)

	// 找一个被标记为 Secret 的设置项，写入一个可识别的明文值。
	var secretKey string
	for _, sp := range settings.Specs() {
		if sp.Secret {
			secretKey = sp.Key
			break
		}
	}
	if secretKey == "" {
		t.Skip("当前没有标记为 Secret 的设置项，跳过")
	}
	const plaintext = "SUPER-SECRET-VALUE-12345"
	pv := plaintext
	if err := db.Create(&model.SystemSetting{
		Key: secretKey, Value: &pv,
	}).Error; err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}

	b, err := svc.Export(context.Background(), []string{diagnostics.CategoryConfig},
		adminUser(), "root", "")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}

	raw := string(b.Data)
	if strings.Contains(raw, plaintext) {
		t.Fatal("诊断包里出现了敏感设置的明文——包会被直接发给别人")
	}
	// zip 是压缩的，明文可能被压掉；解压后再确认一次。
	files := readZip(t, b.Data)
	for name, content := range files {
		if strings.Contains(content, plaintext) {
			t.Errorf("%s 中出现了敏感设置的明文", name)
		}
	}
}

// TestCategorySelection 覆盖按分类导出。
//
// 用户排障时通常只关心某一类；打一个全量包是在浪费他的时间与带宽。
func TestCategorySelection(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	b, err := svc.Export(ctx, []string{diagnostics.CategoryRuntime}, adminUser(), "root", "")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	files := readZip(t, b.Data)
	if _, ok := files["runtime/overview.json"]; !ok {
		t.Error("应包含运行时状态")
	}
	if _, ok := files["config/settings.json"]; ok {
		t.Error("只选了运行时状态时不该包含配置")
	}
	if _, ok := files["logs/audit.json"]; ok {
		t.Error("只选了运行时状态时不该包含日志")
	}
}

// TestTruncationIsRecorded 覆盖"截断必须留痕"。
//
// 不说的话，分析的人会把"只看到了最后 N 条日志"当成"这段时间只有这些"。
func TestTruncationIsRecorded(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	// 写入超过上限的日志。
	rows := make([]model.AuditLog, 0, 2100)
	for i := 0; i < 2100; i++ {
		rows = append(rows, model.AuditLog{Action: "test", Success: true})
	}
	if err := db.CreateInBatches(rows, 500).Error; err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}

	b, err := svc.Export(ctx, []string{diagnostics.CategoryLogs}, adminUser(), "root", "")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if len(b.Truncated) == 0 {
		t.Fatal("超过上限时应报告被截断")
	}
	files := readZip(t, b.Data)
	if !strings.Contains(files["MANIFEST.json"], "truncated") {
		t.Error("截断信息应写进 MANIFEST——包会被离线分析，那时没有界面可看")
	}
	// 响应头里的那份也要有（界面据此提示）。
	if !strings.Contains(strings.Join(b.Truncated, " "), "2000") {
		t.Errorf("截断说明应给出具体条数，实际 %v", b.Truncated)
	}
}

func TestUnknownCategoryRejected(t *testing.T) {
	_, svc := newEnv(t)

	_, err := svc.Export(context.Background(), []string{"不存在的分类"},
		adminUser(), "root", "")
	if err == nil {
		t.Fatal("未知分类应被拒绝")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("应为 400，实际 %v", err)
	}
}

// TestExportIsAudited 覆盖留痕。
//
// 这个包等于把系统的内部状态带走，而它正是排查"谁做了什么"时最有用的东西
// 之一——导出这个动作本身必须记下来。
func TestExportIsAudited(t *testing.T) {
	db, svc := newEnv(t)

	if _, err := svc.Export(context.Background(), nil, adminUser(), "root", "10.0.0.1"); err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	var rec model.AuditLog
	if err := db.Where("action = ?", "diagnostics.export").First(&rec).Error; err != nil {
		t.Fatalf("导出应记审计: %v", err)
	}
	if rec.Params == nil {
		t.Fatal("审计应含参数（选了哪些分类、多大）")
	}
	if rec.ClientIP == nil || *rec.ClientIP != "10.0.0.1" {
		t.Error("审计应记来源 IP")
	}
}

// TestCategoriesDescribeSensitivity 覆盖分类元数据。
//
// 界面要据此在导出前告诉用户"这一类里含什么"——尤其是日志里有别人的
// 操作记录与来源 IP。
func TestCategoriesDescribeSensitivity(t *testing.T) {
	cats := diagnostics.Categories()
	if len(cats) == 0 {
		t.Fatal("应有分类")
	}
	seen := map[string]bool{}
	for _, c := range cats {
		if c.Key == "" || c.Label == "" || c.Description == "" {
			t.Errorf("分类的 key/名称/说明都不能为空: %+v", c)
		}
		seen[c.Key] = true
	}
	if !seen[diagnostics.CategoryLogs] {
		t.Error("应有日志分类")
	}
}

// TestRuntimeOverviewHasNoContent 覆盖"只取计数，不取内容"。
//
// 诊断包要能安全外发，而"有 12 个待执行任务"与"这 12 个任务分别是什么
// （可能含虚拟机名、路径）"是两回事。
func TestRuntimeOverviewHasNoContent(t *testing.T) {
	db, svc := newEnv(t)

	// 造一台虚拟机与一个任务，确认它们的名字**没有**出现在包里。
	if err := db.Create(&model.VM{ID: 1, Name: "租户A的机器", Status: model.VMStatusRunning}).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	tk := model.Task{Type: "vm.start", Status: model.TaskPending}
	if err := db.Create(&tk).Error; err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}

	b, err := svc.Export(context.Background(), []string{diagnostics.CategoryRuntime},
		adminUser(), "root", "")
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	files := readZip(t, b.Data)
	if strings.Contains(files["runtime/overview.json"], "租户A的机器") {
		t.Error("运行时概览不该含虚拟机名——需要细节时用户会去任务中心，那里有权限控制")
	}
	if !strings.Contains(files["runtime/overview.json"], "vms") {
		t.Error("应该含计数")
	}
}
