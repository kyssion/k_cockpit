package template_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/compute/template"
)

// openRecoverDB 打开一个只含 template_export 的临时库。
//
// 建库方式与包内其它用例一致（database.Open + AutoMigrate）。这里只建这一张
// 表：被测函数除它之外不碰任何东西，多建表会掩盖"误改别的表"以外的信号。
func openRecoverDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "recover.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.TemplateExport{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

// seedTemplateExport 插入一行导出记录并返回其 ID。
func seedTemplateExport(t *testing.T, db *gorm.DB, row model.TemplateExport) int64 {
	t.Helper()
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建导出记录失败: %v", err)
	}
	return row.ID
}

// loadTemplateExport 按 ID 重新读一行。
//
// **必须重新读**：断言的对象是库里的状态，不是内存里的结构体。
func loadTemplateExport(t *testing.T, db *gorm.DB, id int64) model.TemplateExport {
	t.Helper()
	var row model.TemplateExport
	if err := db.Where("id = ?", id).First(&row).Error; err != nil {
		t.Fatalf("读取导出记录 %d 失败: %v", id, err)
	}
	return row
}

// TestRecoverInterruptedExportsOnStartup 覆盖「控制面崩溃重启后，影子记录
// 永久卡在 pending」这条缺陷（实机复现路径：POST /templates/2/exports →
// kill -9 → 重启）。
//
// 它要挡住两件事：
//  1. 在途记录（pending / running）必须被收敛成 failed 并带上「请重试」的
//     文案，否则用户永远看到"导出中"，既拿不到包也无法重试；
//  2. 终态记录（success / failed）必须**原样不动**——这是对"全表改 failed"
//     这类假实现的直接拦截：一次重启把历史成功记录改成失败，用户会以为
//     之前导出的包没了。
func TestRecoverInterruptedExportsOnStartup(t *testing.T) {
	db := openRecoverDB(t)

	sha := "9f2c0b7d4a1e"
	oldFinished := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	nodeErr := "节点不可达，导出指令未送达"

	pendingID := seedTemplateExport(t, db, model.TemplateExport{
		NodeID: 1, TemplateID: 2, TemplateName: "interrupted-pending",
		RelPath: "", Filename: "interrupted-pending.tar.gz",
		Status: model.TemplateExportPending,
	})
	runningID := seedTemplateExport(t, db, model.TemplateExport{
		NodeID: 1, TemplateID: 2, TemplateName: "interrupted-running",
		RelPath: "", Filename: "interrupted-running.tar.gz",
		Status: model.TemplateExportRunning,
	})
	successID := seedTemplateExport(t, db, model.TemplateExport{
		NodeID: 1, TemplateID: 2, TemplateName: "done",
		RelPath: "template_export/done.tar.gz", Filename: "done.tar.gz",
		SizeBytes: 1 << 20, SHA256: &sha,
		Status: model.TemplateExportSuccess, FinishedAt: &oldFinished,
	})
	failedID := seedTemplateExport(t, db, model.TemplateExport{
		NodeID: 1, TemplateID: 2, TemplateName: "failed-before-restart",
		RelPath: "", Filename: "failed-before-restart.tar.gz",
		Status: model.TemplateExportFailed, Error: &nodeErr,
		FinishedAt: &oldFinished,
	})

	// 快照终态行：收敛之后逐字段比对，确认它们一点没被碰过。
	successBefore := loadTemplateExport(t, db, successID)

	// 构造服务 == 控制面启动：收敛在构造期执行一次。
	template.NewService(db, nil, nil, nil, nil)

	for _, id := range []int64{pendingID, runningID} {
		row := loadTemplateExport(t, db, id)
		if row.Status != model.TemplateExportFailed {
			t.Errorf("记录 %d 停在 %q，期望收敛为 failed", id, row.Status)
		}
		if row.Error == nil || !strings.Contains(*row.Error, "重启") {
			t.Errorf("记录 %d 的 error = %v，期望包含「重启」以说明原因", id, row.Error)
		}
		if row.FinishedAt == nil {
			t.Errorf("记录 %d 缺少 finished_at", id)
		}
	}

	successAfter := loadTemplateExport(t, db, successID)
	if successAfter.Status != successBefore.Status || successAfter.SizeBytes != successBefore.SizeBytes ||
		derefOr(successAfter.SHA256) != derefOr(successBefore.SHA256) ||
		successAfter.Error != nil {
		t.Errorf("成功的导出记录被改动了: status=%q size=%d sha=%q error=%q",
			successAfter.Status, successAfter.SizeBytes,
			derefOr(successAfter.SHA256), derefOr(successAfter.Error))
	}
	if successAfter.FinishedAt == nil || !successAfter.FinishedAt.Equal(oldFinished) {
		t.Errorf("成功记录的 finished_at 被改动了: %v", successAfter.FinishedAt)
	}

	failedAfter := loadTemplateExport(t, db, failedID)
	if failedAfter.Status != model.TemplateExportFailed {
		t.Errorf("原本失败的记录状态变成了 %q", failedAfter.Status)
	}
	// 原有的失败原因必须留着：它比"重启中断"更接近真相（那是上一次真实失败）。
	if failedAfter.Error == nil || *failedAfter.Error != nodeErr {
		t.Errorf("原有失败记录的 error 被覆盖成 %q", derefOr(failedAfter.Error))
	}
	if failedAfter.FinishedAt == nil || !failedAfter.FinishedAt.Equal(oldFinished) {
		t.Errorf("原有失败记录的 finished_at 被改动了: %v", failedAfter.FinishedAt)
	}

	// 幂等：再启动一次不应报错，也不应改动已经收敛的结果。
	template.NewService(db, nil, nil, nil, nil)
	again := loadTemplateExport(t, db, pendingID)
	if again.Status != model.TemplateExportFailed {
		t.Fatalf("第二次启动把已收敛的记录改成了 %q", again.Status)
	}
	if again.Error == nil || *again.Error != *loadTemplateExport(t, db, runningID).Error {
		t.Errorf("第二次启动改写了 error 文案: %v", again.Error)
	}
	if row := loadTemplateExport(t, db, successID); row.Status != model.TemplateExportSuccess {
		t.Errorf("第二次启动改动了成功记录: %q", row.Status)
	}
}

// derefOr 取可空字符串的值（nil → 空串）。
func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
