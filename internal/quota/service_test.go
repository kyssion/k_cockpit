package quota_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/quota"
)

const gb = 1 << 30

func newTestEnv(t *testing.T) (*quota.Service, *gorm.DB) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "quota.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.UserStorage{}, &model.VM{}, &model.VMExport{},
		&model.Template{}, &model.StorageFile{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return quota.NewService(db, audit.NewRecorder(db)), db
}

// TestUsageIsUnlimitedWithoutRecord 覆盖默认状态。
//
// **不给默认限额**：面板的默认状态应当是「先能用」，限额由管理员显式设置。
// 反过来（默认给一个很小的额度）会让新用户的第一次创建莫名其妙地失败。
func TestUsageIsUnlimitedWithoutRecord(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	u := int64(7)
	db.Create(&model.VM{
		NodeID: 1, Name: "vm-a", OwnerID: &u, DiskGB: 100, Present: true,
	})

	usage, err := svc.Usage(ctx, u, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !usage.Unlimited {
		t.Error("未设配额时应为不限")
	}
	if usage.RemainingBytes != -1 {
		t.Errorf("不限时剩余额度 = %d, 期望 -1（与「还有很多」区分开）", usage.RemainingBytes)
	}
	if usage.VMDisksBytes != 100*gb {
		t.Errorf("虚拟机磁盘 = %d, 期望 %d", usage.VMDisksBytes, 100*gb)
	}

	// 不限时任何创建都放行。
	if err := svc.Check(ctx, u, 1, 9999*gb); err != nil {
		t.Errorf("未设配额时不应拦截: %v", err)
	}
}

// TestCheckBlocksOverQuota 覆盖核心拦截。
//
// 判断用「当前用量 + 本次估算」而不是「当前是否已超」：只判后者会放行一次
// 明显超额的创建，等它落地后才发现——那时用户已经等了几分钟。
func TestCheckBlocksOverQuota(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	db.Create(&model.VM{
		NodeID: 1, Name: "vm-b", OwnerID: &u, DiskGB: 100, Present: true,
	})
	if _, err := svc.Set(ctx, quota.SetQuotaRequest{
		UserID: u, NodeID: 1, Enabled: true, QuotaBytes: 150 * gb,
	}, 1, "admin", ""); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	// 再建一台 100 GB 的：100 + 100 = 200 > 150，应被拦住。
	err := svc.Check(ctx, u, 1, 100*gb)
	if err == nil {
		t.Fatal("超额创建应被拒绝")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Errorf("期望 422 业务错误, 实际 %v", err)
	}

	// 再建一台 40 GB 的：100 + 40 = 140 ≤ 150，应放行。
	if err := svc.Check(ctx, u, 1, 40*gb); err != nil {
		t.Errorf("未超额却被拒绝: %v", err)
	}
}

// TestUsageCountsExportsAndTemplates 覆盖三个来源都计入。
func TestUsageCountsExportsAndTemplates(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	db.Create(&model.VM{NodeID: 1, Name: "vm-c", OwnerID: &u, DiskGB: 10, Present: true})
	db.Create(&model.VMExport{
		VMID: 1, NodeID: 1, VMName: "vm-c", Status: model.ExportSuccess,
		CreatedBy: &u, SizeBytes: 3 * gb,
	})
	// 失败的导出不计入：它没有产物，算进去等于按一次没发生的事收钱。
	db.Create(&model.VMExport{
		VMID: 1, NodeID: 1, VMName: "vm-c", Status: model.ExportFailed,
		CreatedBy: &u, SizeBytes: 99 * gb,
	})
	db.Create(&model.Template{
		NodeID: 1, Name: "tpl-a", Status: model.TemplateReady,
		DiskSizeGB: 20, CreatedBy: &u,
	})

	usage, err := svc.Usage(ctx, u, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if usage.VMDisksBytes != 10*gb {
		t.Errorf("虚拟机磁盘 = %d, 期望 10 GB", usage.VMDisksBytes/gb)
	}
	if usage.ExportsBytes != 3*gb {
		t.Errorf("导出产物 = %d, 期望 3 GB（失败的导出不应计入）", usage.ExportsBytes/gb)
	}
	if usage.TemplatesBytes != 20*gb {
		t.Errorf("模板 = %d, 期望 20 GB", usage.TemplatesBytes/gb)
	}
	if usage.TotalBytes != 33*gb {
		t.Errorf("合计 = %d GB, 期望 33 GB", usage.TotalBytes/gb)
	}
}

// TestRemovedVMsDoNotCount 覆盖一条会让用量「永远降不下来」的边界。
//
// 已不在虚拟化层的虚拟机（present=false）不再占用磁盘，把它们的配置大小
// 继续算进配额，用户会看到一份怎么删都降不下去的用量。
func TestRemovedVMsDoNotCount(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	gone := model.VM{
		NodeID: 1, Name: "vm-gone", OwnerID: &u, DiskGB: 500, Present: true,
	}
	db.Create(&gone)
	// present 的默认值是 true，且 GORM 会省略零值——因此只能走 Update。
	db.Model(&model.VM{}).Where("id = ?", gone.ID).Update("present", false)

	usage, err := svc.Usage(ctx, u, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if usage.VMDisksBytes != 0 {
		t.Errorf("已移除的虚拟机仍被计入配额: %d GB", usage.VMDisksBytes/gb)
	}
}

// TestReadOnlyBlocksEverything 覆盖只读状态。
func TestReadOnlyBlocksEverything(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	if _, err := svc.Set(ctx, quota.SetQuotaRequest{
		UserID: u, NodeID: 1, Enabled: true, QuotaBytes: 100 * gb, ReadOnly: true,
	}, 1, "admin", ""); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	if err := svc.Check(ctx, u, 1, 1*gb); err == nil {
		t.Error("只读状态下不应允许新建资源")
	}
	if err := svc.CheckOverQuota(ctx, u, 1); err == nil {
		t.Error("只读状态下不应允许导出")
	}
}

// TestCheckOverQuotaAllowsWhenNotOver 覆盖弱口径的放行一侧。
//
// 导出这类操作的产物大小**在受理时无法知道**，只能做一次较弱的检查：
// 已经超额就不再放行，未超额则允许。假装能算准只会给出一个看起来精确、
// 实际误导的拒绝理由。
func TestCheckOverQuotaAllowsWhenNotOver(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	db.Create(&model.VM{NodeID: 1, Name: "vm-d", OwnerID: &u, DiskGB: 10, Present: true})
	if _, err := svc.Set(ctx, quota.SetQuotaRequest{
		UserID: u, NodeID: 1, Enabled: true, QuotaBytes: 100 * gb,
	}, 1, "admin", ""); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	if err := svc.CheckOverQuota(ctx, u, 1); err != nil {
		t.Errorf("未超额时不应拦截导出: %v", err)
	}
}

func TestSetQuotaPersistsAndAudits(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	u := int64(7)

	usage, err := svc.Set(ctx, quota.SetQuotaRequest{
		UserID: u, NodeID: 1, Enabled: true, QuotaBytes: 200 * gb,
	}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	if usage.QuotaBytes != 200*gb || usage.Unlimited {
		t.Errorf("返回值未反映新配额: %+v", usage)
	}

	// 再读一次确认落库（而不是只改了内存里那份）。
	again, err := svc.Usage(ctx, u, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if again.QuotaBytes != 200*gb {
		t.Errorf("数据库未持久化配额: %d", again.QuotaBytes)
	}

	var count int64
	db.Model(&model.AuditLog{}).Where("action = ?", "quota.set").Count(&count)
	if count != 1 {
		t.Errorf("审计记录 = %d 条, 期望 1", count)
	}
}
