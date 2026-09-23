package vmtag_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vmtag"
)

func newTestEnv(t *testing.T) (*vmtag.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "tag.db"),
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
		&model.VMTag{}, &model.VM{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return vmtag.NewService(db, audit.NewRecorder(db)), db
}

const alice = int64(7)

func tenant() authz.Viewer { return authz.Viewer{UserID: alice} }
func admin() authz.Viewer  { return authz.Viewer{UserID: 9999, IsAdmin: true} }

func seedVM(t *testing.T, db *gorm.DB, name string, owner int64) *model.VM {
	t.Helper()
	o := owner
	vm := model.VM{NodeID: 1, Name: name, Status: model.VMStatusStopped, OwnerID: &o, Present: true}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return &vm
}

// TestSetTagsReplacesWholesale 覆盖「整体替换」的语义。
//
// 接口是 PUT 而不是 add/remove：界面改完直接保存，而逐个增删会让「加了又删、
// 删了又加」的中间态被如实写进数据库与审计流水，让那条记录读起来很难理解。
func TestSetTagsReplacesWholesale(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-1", alice)

	if _, err := svc.SetTags(ctx, vm.ID, []string{"prod", "db"}, tenant(), "alice", ""); err != nil {
		t.Fatalf("首次保存失败: %v", err)
	}
	// 第二次只给一个标签 → 另一个必须消失。
	got, err := svc.SetTags(ctx, vm.ID, []string{"prod"}, tenant(), "alice", "")
	if err != nil {
		t.Fatalf("第二次保存失败: %v", err)
	}
	if len(got) != 1 || got[0] != "prod" {
		t.Errorf("整体替换未生效: %v", got)
	}

	tags, _ := svc.Tags(ctx, vm.ID, tenant())
	if len(tags) != 1 {
		t.Errorf("库里残留了旧标签: %v", tags)
	}

	// 传空数组 = 清空。
	if _, err := svc.SetTags(ctx, vm.ID, nil, tenant(), "alice", ""); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if tags, _ := svc.Tags(ctx, vm.ID, tenant()); len(tags) != 0 {
		t.Errorf("清空后仍有标签: %v", tags)
	}
}

// TestDuplicateTagsDeduped 覆盖去重。
//
// 唯一索引会拦住重复，但那时错误信息是一句唯一约束冲突，与「你写了两个
// 一样的标签」联系不起来。因此在服务层就去掉。
func TestDuplicateTagsDeduped(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-dup", alice)

	got, err := svc.SetTags(ctx, vm.ID, []string{"a", "b", "a", " a "}, tenant(), "alice", "")
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("去重后 = %v, 期望 2 个", got)
	}
}

func TestTagValidation(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-bad", alice)

	long := strings.Repeat("x", model.MaxTagLen+1)
	for _, bad := range [][]string{{long}, {"a,b"}, {"a\nb"}} {
		_, err := svc.SetTags(ctx, vm.ID, bad, tenant(), "alice", "")
		assertStatus(t, err, 400)
	}
}

// TestTenantCannotTagOthersVM 覆盖越权。
func TestTenantCannotTagOthersVM(t *testing.T) {
	svc, db := newTestEnv(t)
	others := seedVM(t, db, "vm-others", 20)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.SetTags(context.Background(), others.ID, []string{"x"}, tenant(), "alice", "")
	assertStatus(t, err, 404)
}

// TestAllTagsRespectsVisibility 覆盖标签汇总的信息泄漏。
//
// 如果汇总里包含别人的标签，租户就能通过它推断出别人的机器命名习惯与数量。
func TestAllTagsRespectsVisibility(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	mine1 := seedVM(t, db, "mine-1", alice)
	mine2 := seedVM(t, db, "mine-2", alice)
	theirs := seedVM(t, db, "theirs", 20)

	if _, err := svc.SetTags(ctx, mine1.ID, []string{"prod"}, tenant(), "alice", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := svc.SetTags(ctx, mine2.ID, []string{"prod"}, tenant(), "alice", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := svc.SetTags(ctx, theirs.ID, []string{"secret-internal"}, tenant(), "alice", ""); err == nil {
		t.Fatal("不该能标别人的机器")
	}
	// 直接用管理员给别人的机器打标签，模拟"库里确实有别人的标签"。
	if _, err := svc.SetTags(ctx, theirs.ID, []string{"secret-internal"}, admin(), "root", ""); err != nil {
		t.Fatalf("管理员保存失败: %v", err)
	}

	items, err := svc.AllTags(ctx, tenant())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	// 「prod (2)」——计数让用户能判断这个标签是不是一个只有一台机器的孤儿。
	if len(items) != 1 || items[0].Tag != "prod" || items[0].Count != 2 {
		t.Errorf("汇总 = %+v, 期望 prod 计数 2", items)
	}
	for _, it := range items {
		if strings.Contains(it.Tag, "secret") {
			t.Errorf("看到了别人的标签: %v", it)
		}
	}

	// 管理员看得到全部。
	adminItems, _ := svc.AllTags(ctx, admin())
	if len(adminItems) != 2 {
		t.Errorf("管理员应看到 2 个标签, 实际 %d", len(adminItems))
	}
}

// TestVMsByTagRespectsVisibility 覆盖按标签找机器的可见范围。
func TestVMsByTagRespectsVisibility(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	mine := seedVM(t, db, "mine", alice)
	theirs := seedVM(t, db, "theirs", 20)
	for _, vm := range []*model.VM{mine, theirs} {
		if _, err := svc.SetTags(ctx, vm.ID, []string{"shared-tag"}, admin(), "root", ""); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
	}

	ids, err := svc.VMsByTag(ctx, "shared-tag", tenant())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(ids) != 1 || ids[0] != mine.ID {
		t.Errorf("租户应只看到自己的那台: %v", ids)
	}

	all, _ := svc.VMsByTag(ctx, "shared-tag", admin())
	if len(all) != 2 {
		t.Errorf("管理员应看到 2 台, 实际 %d", len(all))
	}
}

// TestSetTagsIsAtomic 覆盖替换的事务性。
//
// 分两步会出现「旧的已删、新的没写」的瞬间，而那段时间里这台机器没有任何
// 标签——从界面上看就是"标签丢了"。
func TestSetTagsIsAtomic(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-atomic", alice)

	if _, err := svc.SetTags(ctx, vm.ID, []string{"a", "b", "c"}, tenant(), "alice", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 换成另一组：结果要么全旧要么全新，不该出现中间态。
	got, err := svc.SetTags(ctx, vm.ID, []string{"x", "y"}, tenant(), "alice", "")
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("结果 = %v", got)
	}
	var n int64
	db.Model(&model.VMTag{}).Where("vm_id = ?", vm.ID).Count(&n)
	if n != 2 {
		t.Errorf("库里 = %d 条, 期望恰好 2 条（既没有残留也没有缺失）", n)
	}
}

// TestTagsAreSorted 覆盖输出的确定性。
//
// 顺序会变会让界面看起来在闪烁，而排序让同一组标签每次渲染一致。
func TestTagsAreSorted(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-sort", alice)

	got, _ := svc.SetTags(ctx, vm.ID, []string{"z", "a", "m"}, tenant(), "alice", "")
	want := []string{"a", "m", "z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v, 期望 %v", got, want)
		}
	}
}

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
