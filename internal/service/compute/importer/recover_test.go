package importer_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/compute/importer"
)

// openRecoverDB 打开一个只含 image_import 的临时库。
//
// 建库方式与包内其它用例一致（database.Open + AutoMigrate）；只建被测的那张表。
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
	if err := db.AutoMigrate(&model.ImageImport{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

// seedImageImport 插入一行导入记录并返回其 ID。
func seedImageImport(t *testing.T, db *gorm.DB, row model.ImageImport) int64 {
	t.Helper()
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建导入记录失败: %v", err)
	}
	return row.ID
}

// loadImageImport 按 ID 重新读一行（断言的对象是库里的状态）。
func loadImageImport(t *testing.T, db *gorm.DB, id int64) model.ImageImport {
	t.Helper()
	var row model.ImageImport
	if err := db.Where("id = ?", id).First(&row).Error; err != nil {
		t.Fatalf("读取导入记录 %d 失败: %v", id, err)
	}
	return row
}

// TestRecoverInterruptedImportsOnStartup 覆盖 image_import 上的同一条缺陷：
// 记录在入队前就建行（ImportPending），执行器改成 ImportRunning 后由终态
// 写回；进程被 kill -9 时执行器不会跑，记录永久停在 pending/running ——
// 界面一直显示"导入中"，用户既拿不到产物模板，连记录都删不掉（Delete 会以
// "仍在进行中"拒绝）。
//
// 两条断言缺一不可：
//  1. pending / running 收敛为 failed 且 error 说明「重启」；
//  2. success / failed **原样不动**（挡住"全表改 failed"的假实现）。
func TestRecoverInterruptedImportsOnStartup(t *testing.T) {
	db := openRecoverDB(t)

	oldFinished := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Second)
	templateID := int64(42)
	diskPath := "/var/lib/k_cockpit/images/imported.qcow2"
	preview := `{"vcpu":2,"memory_mb":2048,"disk_gb":20,"sources":{"vcpu":"user"}}`
	nodeErr := "源文件校验失败"

	pendingID := seedImageImport(t, db, model.ImageImport{
		NodeID: 1, Name: "ubuntu-pending",
		SourceFilename: "ubuntu.qcow2", SourceFormat: model.ImportQCOW2,
		Status: model.ImportPending,
	})
	runningID := seedImageImport(t, db, model.ImageImport{
		NodeID: 1, Name: "ubuntu-running",
		SourceFilename: "ubuntu.qcow2", SourceFormat: model.ImportQCOW2,
		Status: model.ImportRunning,
	})
	successID := seedImageImport(t, db, model.ImageImport{
		NodeID: 1, Name: "ubuntu-done",
		SourceFilename: "ubuntu.qcow2", SourceFormat: model.ImportQCOW2,
		Status: model.ImportSuccess, DiskPath: &diskPath, TemplateID: &templateID,
		Preview: &preview, FinishedAt: &oldFinished,
	})
	failedID := seedImageImport(t, db, model.ImageImport{
		NodeID: 1, Name: "ubuntu-failed",
		SourceFilename: "ubuntu.qcow2", SourceFormat: model.ImportQCOW2,
		Status: model.ImportFailed, Error: &nodeErr, FinishedAt: &oldFinished,
	})

	// 快照终态行：收敛之后逐字段比对，确认它们一点没被碰过。
	successBefore := loadImageImport(t, db, successID)

	// 构造服务 == 控制面启动：收敛在构造期执行一次。
	importer.NewService(db, nil, nil, nil, nil)

	for _, id := range []int64{pendingID, runningID} {
		row := loadImageImport(t, db, id)
		if row.Status != model.ImportFailed {
			t.Errorf("记录 %d 停在 %q，期望收敛为 failed", id, row.Status)
		}
		if row.Error == nil || !strings.Contains(*row.Error, "重启") {
			t.Errorf("记录 %d 的 error = %v，期望包含「重启」", id, row.Error)
		}
		if row.FinishedAt == nil {
			t.Errorf("记录 %d 缺少 finished_at", id)
		}
	}

	successAfter := loadImageImport(t, db, successID)
	if successAfter.Status != successBefore.Status {
		t.Errorf("成功的导入记录状态被改成了 %q", successAfter.Status)
	}
	// 产物（模板 ID 与磁盘路径）是"导入成功"的实际含义，重启收敛绝不能抹掉它。
	if successAfter.TemplateID == nil || *successAfter.TemplateID != templateID ||
		successAfter.DiskPath == nil || *successAfter.DiskPath != diskPath {
		t.Errorf("成功记录的产物信息被改动了: template_id=%v disk_path=%q",
			successAfter.TemplateID, strOf(successAfter.DiskPath))
	}
	if successAfter.Error != nil {
		t.Errorf("成功记录被写入了 error: %v", *successAfter.Error)
	}
	if successAfter.Preview == nil || *successAfter.Preview != preview {
		t.Errorf("成功记录的预览被改动了: %v", successAfter.Preview)
	}
	if successAfter.FinishedAt == nil || !successAfter.FinishedAt.Equal(oldFinished) {
		t.Errorf("成功记录的 finished_at 被改动了: %v", successAfter.FinishedAt)
	}

	failedAfter := loadImageImport(t, db, failedID)
	if failedAfter.Status != model.ImportFailed {
		t.Errorf("原本失败的记录状态变成了 %q", failedAfter.Status)
	}
	// 原有的失败原因必须留着：它比"重启中断"更接近真相。
	if failedAfter.Error == nil || *failedAfter.Error != nodeErr {
		t.Errorf("原有失败记录的 error 被覆盖成 %q", strOf(failedAfter.Error))
	}
	if failedAfter.FinishedAt == nil || !failedAfter.FinishedAt.Equal(oldFinished) {
		t.Errorf("原有失败记录的 finished_at 被改动了: %v", failedAfter.FinishedAt)
	}

	// 幂等：再启动一次不报错，也不改写已收敛的结果。
	importer.NewService(db, nil, nil, nil, nil)
	if row := loadImageImport(t, db, pendingID); row.Status != model.ImportFailed {
		t.Fatalf("第二次启动把已收敛的记录改成了 %q", row.Status)
	}
	if row := loadImageImport(t, db, successID); row.Status != model.ImportSuccess {
		t.Errorf("第二次启动改动了成功记录: %q", row.Status)
	}
}

// strOf 取可空字符串的值（nil → 空串），只用于测试失败时的可读输出。
func strOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
