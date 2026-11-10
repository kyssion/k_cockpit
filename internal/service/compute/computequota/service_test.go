package computequota_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/compute/computequota"
)

func newEnv(t *testing.T) (*gorm.DB, *computequota.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "cq.db"),
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
	// 数量型维度要连 vm_snapshot / port_forward / public_ip_binding，
	// 因此这几张表也要建出来——配额统计现在依赖它们。
	if err := db.AutoMigrate(&model.ComputeQuota{}, &model.VM{}, &model.User{}, &model.Node{},
		&model.AuditLog{},
		&model.VMSnapshot{}, &model.PortForward{}, &model.PublicIPBinding{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 配额挂在 (节点, 用户) 上，Set 会核对两个归属对象确实存在。测试库统一
	// 预置它们，避免每个用例都在重复同一段与本用例无关的 fixture。
	for _, seed := range []any{
		&model.Node{ID: 1, Name: "test-node"},
		&model.User{ID: 7, Username: "alice"},
		&model.User{ID: 8, Username: "bob"},
	} {
		if err := db.Create(seed).Error; err != nil {
			t.Fatalf("预置节点/用户失败: %v", err)
		}
	}
	return db, computequota.NewService(db, nil)
}

// limits 构造三个计算维度上限，数量型维度留 0（不限）。
func limits(vcpu, memMB, vms int) computequota.Limits {
	return computequota.Limits{VCPU: vcpu, MemoryMB: memMB, VMCount: vms}
}

// add 构造"本次新增"，数量型维度留 0。
func add(vms, vcpu, memMB int) computequota.Additions {
	return computequota.Additions{VMs: vms, VCPU: vcpu, MemoryMB: memMB}
}

func seedVM(t *testing.T, db *gorm.DB, name string, owner int64, vcpu, memoryMB int) {
	t.Helper()
	vm := model.VM{
		NodeID: 1, Name: name, OwnerID: &owner, Present: true,
		VCPU: vcpu, MemoryMB: memoryMB,
	}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
}

// TestCheckRejectsWhenOverLimit 覆盖超限拦截，以及**报错里必须有余量**。
//
// 只说"超出配额"会让用户去猜是哪个维度超了、差多少，而他此刻正卡在创建
// 这一步上，没有任何别的信息可用。
func TestCheckRejectsWhenOverLimit(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	if err := svc.Set(ctx, 1, 7, limits(4, 4096, 2), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	seedVM(t, db, "vm-1", 7, 2, 2048)

	// 再建一台 4 核：vCPU 会到 6，超过上限 4。
	err := svc.Check(ctx, 7, 1, add(1, 4, 4096))
	if err == nil {
		t.Fatal("超出 vCPU 上限未被拒绝")
	}
	if !strings.Contains(err.Error(), "vCPU") || !strings.Contains(err.Error(), "还剩") {
		t.Errorf("错误未指出维度与余量: %v", err)
	}

	// 台数超限：已有 1 台 + 再建 2 台 = 3 > 2。
	if err := svc.Check(ctx, 7, 1, add(2, 1, 512)); err == nil {
		t.Fatal("超出实例数上限未被拒绝")
	} else if !strings.Contains(err.Error(), "虚拟机数量") {
		t.Errorf("错误未指出是实例数超限: %v", err)
	}

	// 在余量之内应当放行。
	if err := svc.Check(ctx, 7, 1, add(1, 2, 2048)); err != nil {
		t.Errorf("余量之内的创建被拒绝: %v", err)
	}
}

// TestNoQuotaMeansUnlimited 覆盖「没有配额 = 不限」。
func TestNoQuotaMeansUnlimited(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	if err := svc.Check(ctx, 7, 1, add(10, 64, 131072)); err != nil {
		t.Errorf("未设置配额时不应拦截: %v", err)
	}
}

// TestZeroClearsQuota 覆盖「三个上限全为 0 = 删除配额」。
//
// 这是一种**状态而不是数字**：把三项都清零在语义上等于"不限制"，而不是
// "限制为 0"——后者会让用户一台都建不了，且没有任何地方能解释。
func TestZeroClearsQuota(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	if err := svc.Set(ctx, 1, 7, limits(4, 4096, 2), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	if err := svc.Set(ctx, 1, 7, limits(0, 0, 0), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("清空配额失败: %v", err)
	}
	var n int64
	db.Model(&model.ComputeQuota{}).Count(&n)
	if n != 0 {
		t.Errorf("清空后配额记录 = %d, 期望 0", n)
	}
	if err := svc.Check(ctx, 7, 1, add(10, 64, 131072)); err != nil {
		t.Errorf("清空后应视为不限: %v", err)
	}
}

// TestListIncludesUsersWithoutQuota 覆盖并集：有占用但没配额的用户也要出现。
//
// 否则管理员看不到"谁在用、但没配过额度"——而那正是他要设置配额的对象。
func TestListIncludesUsersWithoutQuota(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	if err := db.FirstOrCreate(&model.User{ID: 7, Username: "alice"},
		&model.User{ID: 7}).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	seedVM(t, db, "vm-1", 7, 2, 2048)
	if err := svc.Set(ctx, 1, 8, limits(8, 8192, 4), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	items, err := svc.List(ctx, 1)
	if err != nil {
		t.Fatalf("列出配额失败: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("条目数 = %d, 期望 2（一个有配额、一个只有用量）", len(items))
	}
	byUser := map[int64]computequota.View{}
	for _, it := range items {
		byUser[it.UserID] = it
	}
	// 7 号只有用量、没配额度；8 号只有额度、一台机器都没有。
	if byUser[7].HasQuota {
		t.Error("未配额度的用户被误标为有配额")
	}
	if byUser[7].VCPU != 2 || byUser[7].VMCount != 1 {
		t.Errorf("用量 = %+v, 期望 2 核 / 1 台", byUser[7].Usage)
	}
	if !byUser[8].HasQuota {
		t.Error("配了额度的用户被漏掉——管理员会以为那条配额没生效")
	}
	if byUser[8].VMCount != 0 || byUser[8].QuotaVCPU != 8 {
		t.Errorf("仅有配额的那一行 = %+v", byUser[8])
	}
	if byUser[7].Username != "alice" {
		t.Errorf("用户名 = %q, 期望 alice", byUser[7].Username)
	}
}

// TestUsageIgnoresMissingVMs 覆盖「失效机器不占额度」。
func TestUsageIgnoresMissingVMs(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	seedVM(t, db, "alive", 7, 2, 2048)
	seedVM(t, db, "gone", 7, 8, 8192)
	if err := db.Model(&model.VM{}).Where("name = ?", "gone").
		Update("present", false).Error; err != nil {
		t.Fatalf("置失效失败: %v", err)
	}
	if err := svc.Set(ctx, 1, 7, limits(2, 2048, 1), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	// 上限正好等于现存那台；若把失效的那台也算进来，这里会被拒。
	if err := svc.Check(ctx, 7, 1, add(0, 0, 0)); err != nil {
		t.Errorf("失效机器不应占额度: %v", err)
	}
}

// authz 未直接使用，但保留导入以标明这些校验**在服务端**生效：
// 界面上的禁用只是体验优化，不构成安全边界（f-1-06 R-004）。
var _ = authz.Viewer{}

// TestSetRejectsMissingTargets 给不存在的节点或用户写配额必须拒绝：
// 否则会留下界面上无主、又不随任何资源删除的孤儿记录。真机第 12 轮
// 用 user_id=999999999 复现过：返回 200 且列表里多出一条空用户名配额。
func TestSetRejectsMissingTargets(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	if err := svc.Set(ctx, 1, 7, limits(2, 1024, 1), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("正常设置配额失败: %v", err)
	}
	for _, tc := range []struct {
		name string
		node int64
		user int64
	}{
		{"用户不存在", 1, 999},
		{"节点不存在", 999, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.Set(ctx, tc.node, tc.user, limits(2, 1024, 1), 1, "root", "10.0.0.1")
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
				t.Fatalf("期望 404，实际 %v", err)
			}
		})
	}
	var orphans int64
	if err := db.Model(&model.ComputeQuota{}).
		Where("user_id = 999 OR node_id = 999").Count(&orphans).Error; err != nil {
		t.Fatalf("统计孤儿配额失败: %v", err)
	}
	if orphans != 0 {
		t.Errorf("拒绝后仍写入了 %d 条孤儿配额", orphans)
	}

	// 删除路径不校验目标存在：历史遗留的孤儿记录要能被清掉。
	if err := db.Create(&model.ComputeQuota{NodeID: 1, UserID: 888, VCPU: 1}).Error; err != nil {
		t.Fatalf("插入历史孤儿记录失败: %v", err)
	}
	if err := svc.Set(ctx, 1, 888, limits(0, 0, 0), 1, "root", "10.0.0.1"); err != nil {
		t.Fatalf("清理历史孤儿配额失败: %v", err)
	}
	var left int64
	db.Model(&model.ComputeQuota{}).Where("user_id = 888").Count(&left)
	if left != 0 {
		t.Errorf("历史孤儿配额未被清掉，仍有 %d 条", left)
	}
}
