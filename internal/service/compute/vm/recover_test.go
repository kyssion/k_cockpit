package vm_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/compute/vm"
)

// openRecoverDB 打开一个只含 vm_export 的临时库。
//
// 建库方式与包内其它用例一致（database.Open + AutoMigrate）；只建被测的那张表，
// 让"误改了别的表"以外的问题无处藏身。
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
	if err := db.AutoMigrate(&model.VMExport{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

// seedVMExport 插入一行导出记录并返回其 ID。
func seedVMExport(t *testing.T, db *gorm.DB, row model.VMExport) int64 {
	t.Helper()
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建导出记录失败: %v", err)
	}
	return row.ID
}

// loadVMExport 按 ID 重新读一行（断言的对象是库里的状态）。
func loadVMExport(t *testing.T, db *gorm.DB, id int64) model.VMExport {
	t.Helper()
	var row model.VMExport
	if err := db.Where("id = ?", id).First(&row).Error; err != nil {
		t.Fatalf("读取导出记录 %d 失败: %v", id, err)
	}
	return row
}

// TestRecoverInterruptedExportsOnStartup 覆盖 vm_export 上同一条缺陷：
// 导出记录在入队前就建行（pending），终态由 export_executor 写回；进程被
// kill -9 后执行器不会跑，记录永久停在 pending —— 界面一直显示"导出中"，
// 用户拿不到产物，DeleteExport 还会以"仍在进行中"拒绝删除。
//
// 两条断言缺一不可：
//  1. pending / running 收敛为 failed 且 error 说明「重启」；
//  2. success / failed **原样不动**（挡住"全表改 failed"的假实现）。
func TestRecoverInterruptedExportsOnStartup(t *testing.T) {
	db := openRecoverDB(t)

	oldFinished := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	fileName := "web-01.qcow2"
	filePath := "/var/lib/k_cockpit/exports/web-01.qcow2"
	nodeErr := "节点不可达，导出指令未送达"

	pendingID := seedVMExport(t, db, model.VMExport{
		VMID: 1, NodeID: 1, VMName: "web-01", Format: model.ExportQCOW2,
		Status: model.ExportPending,
	})
	runningID := seedVMExport(t, db, model.VMExport{
		VMID: 2, NodeID: 1, VMName: "web-02", Format: model.ExportOVA,
		Status: model.ExportRunning,
	})
	successID := seedVMExport(t, db, model.VMExport{
		VMID: 3, NodeID: 1, VMName: "db-01", Format: model.ExportQCOW2,
		Status: model.ExportSuccess, FileName: &fileName, FilePath: &filePath,
		SizeBytes: 4 << 20, FinishedAt: &oldFinished,
	})
	failedID := seedVMExport(t, db, model.VMExport{
		VMID: 4, NodeID: 1, VMName: "db-02", Format: model.ExportQCOW2,
		Status: model.ExportFailed, Error: &nodeErr, FinishedAt: &oldFinished,
	})

	successBefore := loadVMExport(t, db, successID)

	// 构造服务 == 控制面启动：收敛在构造期执行一次。
	vm.NewService(db, nil, nil, nil, nil, nil)

	for _, id := range []int64{pendingID, runningID} {
		row := loadVMExport(t, db, id)
		if row.Status != model.ExportFailed {
			t.Errorf("记录 %d 停在 %q，期望收敛为 failed", id, row.Status)
		}
		if row.Error == nil || !strings.Contains(*row.Error, "重启") {
			t.Errorf("记录 %d 的 error = %v，期望包含「重启」", id, row.Error)
		}
		if row.FinishedAt == nil {
			t.Errorf("记录 %d 缺少 finished_at", id)
		}
	}

	successAfter := loadVMExport(t, db, successID)
	if successAfter.Status != successBefore.Status ||
		successAfter.SizeBytes != successBefore.SizeBytes ||
		successAfter.Error != nil {
		t.Errorf("成功的导出记录被改动了: status=%q size=%d error=%q",
			successAfter.Status, successAfter.SizeBytes, strOf(successAfter.Error))
	}
	// 产物路径与文件名是下载的依据，重启收敛绝不能把它们抹掉。
	if successAfter.FilePath == nil || *successAfter.FilePath != filePath ||
		successAfter.FileName == nil || *successAfter.FileName != fileName {
		t.Errorf("成功记录的产物路径/文件名被改动了: %+v", successAfter)
	}
	if successAfter.FinishedAt == nil || !successAfter.FinishedAt.Equal(oldFinished) {
		t.Errorf("成功记录的 finished_at 被改动了: %v", successAfter.FinishedAt)
	}

	failedAfter := loadVMExport(t, db, failedID)
	if failedAfter.Status != model.ExportFailed {
		t.Errorf("原本失败的记录状态变成了 %q", failedAfter.Status)
	}
	if failedAfter.Error == nil || *failedAfter.Error != nodeErr {
		t.Errorf("原有失败记录的 error 被覆盖成 %q", strOf(failedAfter.Error))
	}
	if failedAfter.FinishedAt == nil || !failedAfter.FinishedAt.Equal(oldFinished) {
		t.Errorf("原有失败记录的 finished_at 被改动了: %v", failedAfter.FinishedAt)
	}

	// 幂等：再启动一次不报错，也不改写已收敛的结果。
	vm.NewService(db, nil, nil, nil, nil, nil)
	if row := loadVMExport(t, db, pendingID); row.Status != model.ExportFailed {
		t.Fatalf("第二次启动把已收敛的记录改成了 %q", row.Status)
	}
	if row := loadVMExport(t, db, runningID); row.Status != model.ExportFailed {
		t.Fatalf("第二次启动把已收敛的记录改成了 %q", row.Status)
	}
	if row := loadVMExport(t, db, successID); row.Status != model.ExportSuccess {
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
