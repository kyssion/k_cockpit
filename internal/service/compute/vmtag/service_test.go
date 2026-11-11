package vmtag_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	vmtagrepo "k_cockpit/internal/repository/vmtag"
	"k_cockpit/internal/service/compute/vmtag"
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
	return vmtag.NewService(vmtagrepo.NewRepo(db), audit.NewRecorder(db)), db
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

// TestTagsOfBatchesAcrossVMs 覆盖批量读取（TagsOf）。
//
// 这个路径存在的理由就是避免 N+1：列表一页 20 台，用单台的 Tags 循环调用
// 会把一次列表请求变成 20 次数据库往返。它此前只有单台路径被测，批量路径
// 在 `-coverpkg` 口径下是 0%。
//
// 断言的三个语义点：
//  1. 每台机器的标签完整且按字典序（界面按返回值直接渲染）；
//  2. **没有标签的机器不出现在 map 里**——调用方按缺失处理，而不是拿到
//     一个空切片：空切片与「没查到」在 JSON 里都是 []，但语义不同；
//  3. 不存在的 vm_id 不报错，也不出现在结果里。
func TestTagsOfBatchesAcrossVMs(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	a := seedVM(t, db, "batch-a", alice)
	b := seedVM(t, db, "batch-b", alice)
	c := seedVM(t, db, "batch-empty", alice) // 刻意不打标签

	if _, err := svc.SetTags(ctx, a.ID, []string{"prod", "db"}, tenant(), "alice", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := svc.SetTags(ctx, b.ID, []string{"staging"}, tenant(), "alice", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got, err := svc.TagsOf(ctx, []int64{a.ID, b.ID, c.ID, 999999})
	if err != nil {
		t.Fatalf("批量查询失败: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("结果应只含 2 台有标签的机器，实际 %d 台: %v", len(got), got)
	}
	// 字典序：db 在 prod 之前。
	if want := []string{"db", "prod"}; !equalTags(got[a.ID], want) {
		t.Errorf("vm a 标签 = %v, 期望 %v", got[a.ID], want)
	}
	if want := []string{"staging"}; !equalTags(got[b.ID], want) {
		t.Errorf("vm b 标签 = %v, 期望 %v", got[b.ID], want)
	}
	if _, ok := got[c.ID]; ok {
		t.Errorf("没标签的机器不该出现在结果里: %v", got[c.ID])
	}
	if _, ok := got[999999]; ok {
		t.Error("不存在的 vm_id 不该出现在结果里")
	}
}

// TestTagsOfWithEmptyInputSkipsQuery 覆盖空入参。
//
// 返回空 map（而不是 nil）：调用方直接做 `m[id]` 取值，nil map 读取虽然
// 安全，但把它传回去再写入就会 panic。空入参不该产生一次 `IN ()` 查询
// ——那在某些数据库上是语法错误。
func TestTagsOfWithEmptyInputSkipsQuery(t *testing.T) {
	svc, _ := newTestEnv(t)

	got, err := svc.TagsOf(context.Background(), nil)
	if err != nil {
		t.Fatalf("空入参不该出错: %v", err)
	}
	if got == nil {
		t.Fatal("应返回空 map 而不是 nil")
	}
	if len(got) != 0 {
		t.Errorf("空入参结果应为空，实际 %v", got)
	}

	// 空切片与 nil 等价。
	if got, err := svc.TagsOf(context.Background(), []int64{}); err != nil || len(got) != 0 {
		t.Errorf("空切片结果 = %v, err = %v", got, err)
	}
}

// TestTagsOfHidesNothingAcrossOwners 记录批量路径**不做**可见性过滤这一事实。
//
// TagsOf 是内部批量读取，可见性由调用方在拿到机器列表时就已判定（列表本身
// 只含调用者可见的机器）。这条用例固定住这个契约：如果将来有人在 TagsOf
// 里加可见性过滤，它的签名就必须多一个 Viewer 参数——那时这条用例会编译
// 失败，提醒改动者这是接口变更而不是内部优化。
func TestTagsOfHidesNothingAcrossOwners(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	mine := seedVM(t, db, "mine", alice)
	theirs := seedVM(t, db, "theirs", 20)
	if _, err := svc.SetTags(ctx, theirs.ID, []string{"other"}, admin(), "root", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	got, err := svc.TagsOf(ctx, []int64{mine.ID, theirs.ID})
	if err != nil {
		t.Fatalf("批量查询失败: %v", err)
	}
	if want := []string{"other"}; !equalTags(got[theirs.ID], want) {
		t.Errorf("批量路径按 id 直读，应能取到别人的标签（由调用方负责范围）: %v", got)
	}
}

// TestStorageFailureNeverLeaksAsSuccess 覆盖数据层不可用时的降级。
//
// 每一条读写路径都必须把「查不到」与「查出错」分开：前者是 404，后者是
// 500。若把错误吞掉返回空结果，界面会显示「这台机器没有标签」——一个
// 看起来正常的错误答案，比一个明确的失败危险得多。
//
// 用关闭连接制造真实的存储故障（而不是打桩替换 repo），因为要验的正是
// repository 返回错误之后服务层怎么处理。
func TestStorageFailureNeverLeaksAsSuccess(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-broken", alice)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("关闭连接失败: %v", err)
	}

	if _, err := svc.TagsOf(ctx, []int64{vm.ID}); err == nil {
		t.Error("批量读在存储故障时应返回错误")
	} else {
		assertStatus(t, err, 500)
	}
	if _, err := svc.Tags(ctx, vm.ID, tenant()); err == nil {
		t.Error("单台读在存储故障时应返回错误")
	} else {
		assertStatus(t, err, 500)
	}
	if _, err := svc.SetTags(ctx, vm.ID, []string{"x"}, tenant(), "alice", ""); err == nil {
		t.Error("写在存储故障时应返回错误")
	} else {
		assertStatus(t, err, 500)
	}
	if _, err := svc.VMsByTag(ctx, "x", tenant()); err == nil {
		t.Error("按标签查询在存储故障时应返回错误")
	} else {
		assertStatus(t, err, 500)
	}
	if _, err := svc.AllTags(ctx, tenant()); err == nil {
		t.Error("标签汇总在存储故障时应返回错误")
	} else {
		assertStatus(t, err, 500)
	}
}

// TestServiceWithoutAuditRecorder 覆盖未装配审计记录器的部署形态。
//
// audit 为 nil 时录审计必须是空操作而不是 panic：轻量部署与部分测试会
// 这样装配，而「写标签」不该因为没接审计就崩掉。
func TestServiceWithoutAuditRecorder(t *testing.T) {
	_, db := newTestEnv(t)
	svc := vmtag.NewService(vmtagrepo.NewRepo(db), nil)
	ctx := context.Background()
	vm := seedVM(t, db, "vm-noaudit", alice)

	got, err := svc.SetTags(ctx, vm.ID, []string{"solo"}, tenant(), "alice", "")
	if err != nil {
		t.Fatalf("未接审计时保存失败: %v", err)
	}
	if want := []string{"solo"}; !equalTags(got, want) {
		t.Errorf("标签 = %v, 期望 %v", got, want)
	}
}

func equalTags(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
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
