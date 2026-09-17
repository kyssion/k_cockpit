package auditlog_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auditlog"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

func newTestEnv(t *testing.T) (*auditlog.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "audit.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return auditlog.NewService(db), db
}

const (
	alice = int64(7)
	bob   = int64(8)
)

// seed 写一条审计记录。
func seed(t *testing.T, db *gorm.DB, operator int64, action, resType, resName string, success bool) {
	t.Helper()
	var op *int64
	if operator > 0 {
		op = &operator
	}
	name := resName
	row := model.AuditLog{
		At: time.Now(), OperatorID: op, Source: "web",
		ResourceType: resType, ResourceName: &name, Action: action, Success: success,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("写入审计记录失败: %v", err)
	}
}

func tenant(id int64) authz.Viewer { return authz.Viewer{UserID: id} }
func admin() authz.Viewer          { return authz.Viewer{UserID: 1, IsAdmin: true} }

// TestTenantSeesOnlyOwnRecords 是本包最重要的一条。
//
// 审计条目里带着资源名、客户端 IP、操作参数与前后状态。把这些暴露给别的
// 租户，等于把「谁在什么时候动过什么」这张图交出去——而「对方有一台叫
// prod-db 的机器」这类信息本身就有价值。
func TestTenantSeesOnlyOwnRecords(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, alice, "vm.delete", "vm", "alice-vm", true)
	seed(t, db, bob, "vm.delete", "vm", "bob-vm", true)
	seed(t, db, bob, "vm.create", "vm", "bob-vm2", true)

	page, err := svc.List(ctx, auditlog.Filter{}, tenant(alice))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("alice 看到了 %d 条, 期望 1", page.Total)
	}
	if page.Items[0].ResourceName != "alice-vm" {
		t.Errorf("看到了别人的记录: %s", page.Items[0].ResourceName)
	}
}

// TestTenantCannotOverrideOperatorFilter 覆盖一处「靠自己人自觉」的写法。
//
// 把隔离寄托在「界面不会传别人的 operator_id」上是不行的——那一条判断必须
// 在服务层，而且之后无论谁加新的查询入口都绕不过它。
func TestTenantCannotOverrideOperatorFilter(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, alice, "vm.delete", "vm", "alice-vm", true)
	seed(t, db, bob, "vm.delete", "vm", "bob-vm", true)

	// 租户显式传 bob 的 id，仍然只能看到自己的。
	page, err := svc.List(ctx, auditlog.Filter{OperatorID: bob}, tenant(alice))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 || page.Items[0].ResourceName != "alice-vm" {
		t.Errorf("租户传 operator_id 越过了隔离: %+v", page.Items)
	}
}

// TestSystemActionsHiddenFromTenants 覆盖系统动作的可见性。
//
// operator_id 为空的记录是系统自己的动作（如自动关机、看门狗回滚）。
// 它们不属于任何租户，因此对租户不可见。
func TestSystemActionsHiddenFromTenants(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, 0, "vm.auto_shutdown", "vm", "sys-vm", true)
	seed(t, db, alice, "vm.delete", "vm", "alice-vm", true)

	page, err := svc.List(ctx, auditlog.Filter{}, tenant(alice))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("租户看到了 %d 条, 期望只有自己的 1 条", page.Total)
	}

	// 管理员能看到系统动作——那正是排查自动行为时要看的东西。
	adminPage, err := svc.List(ctx, auditlog.Filter{}, admin())
	if err != nil {
		t.Fatalf("管理员查询失败: %v", err)
	}
	if adminPage.Total != 2 {
		t.Errorf("管理员看到了 %d 条, 期望 2", adminPage.Total)
	}
}

// TestGetOthersRecordReturns404 覆盖信息泄漏的一个细节。
//
// 返回 403 会确认「这个 id 存在」，让人能通过枚举推断出系统里有多少条记录、
// 以及它们属于谁。
func TestGetOthersRecordReturns404(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, bob, "vm.delete", "vm", "bob-vm", true)

	var row model.AuditLog
	db.First(&row)

	_, err := svc.Get(ctx, row.ID, tenant(alice))
	assertStatus(t, err, 404)

	// 自己的记录要能看到——只测前者拦不住一个「全都看不到」的实现。
	seed(t, db, alice, "vm.create", "vm", "alice-vm", true)
	var own model.AuditLog
	db.Where("operator_id = ?", alice).First(&own)
	if _, err := svc.Get(ctx, own.ID, tenant(alice)); err != nil {
		t.Errorf("自己的记录应可读取: %v", err)
	}
}

// TestActionPrefixMatch 覆盖分层命名下的筛选。
//
// 动作名是分层的（vm.snapshot.create），而用户想看的往往是「所有快照相关的
// 动作」。要求他逐个列出十几个精确名称才能看全，等于让筛选形同虚设。
func TestActionPrefixMatch(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, alice, "vm.snapshot.create", "vm", "a", true)
	seed(t, db, alice, "vm.snapshot.delete", "vm", "b", true)
	seed(t, db, alice, "vm.delete", "vm", "c", true)

	page, err := svc.List(ctx, auditlog.Filter{Action: "vm.snapshot."}, admin())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("前缀匹配返回 %d 条, 期望 2", page.Total)
	}
}

// TestLikeWildcardsEscaped 覆盖一处看起来像「筛选没生效」的缺陷。
//
// 不转义的话，用户搜 `%` 会命中一切——而他会以为筛选坏了。
func TestLikeWildcardsEscaped(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, alice, "vm.delete", "vm", "normal-name", true)
	seed(t, db, alice, "vm.delete", "vm", "100%cpu", true)

	page, err := svc.List(ctx, auditlog.Filter{Keyword: "%"}, admin())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("关键字 %%%% 匹配到 %d 条, 期望 1（只应是名字里真的含 %% 的那条）", page.Total)
	}
}

// TestKeywordOnlySearchesNameAndError 覆盖匹配范围。
//
// 把参数也纳入匹配，会让「搜一个常见的短词」返回一堆看起来无关的记录。
func TestKeywordOnlySearchesNameAndError(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	name := "worker-01"
	params := `{"note":"worker-01"}`
	row := model.AuditLog{
		At: time.Now(), OperatorID: ptr(alice), Source: "web",
		ResourceType: "vm", ResourceName: &name, Action: "vm.create",
		Params: &params, Success: true,
	}
	db.Create(&row)

	// 资源名匹配得到。
	if p, _ := svc.List(ctx, auditlog.Filter{Keyword: "worker"}, admin()); p.Total != 1 {
		t.Error("应按资源名匹配到")
	}
	// 只有参数里出现的词匹配不到。
	if p, _ := svc.List(ctx, auditlog.Filter{Keyword: "note"}, admin()); p.Total != 0 {
		t.Error("不该在参数里匹配——那会让搜索结果看起来无关")
	}
}

// TestSuccessFilter 覆盖三态筛选。
func TestSuccessFilter(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, alice, "vm.create", "vm", "ok", true)
	seed(t, db, alice, "vm.create", "vm", "bad", false)

	yes, no := true, false
	if p, _ := svc.List(ctx, auditlog.Filter{Success: &yes}, admin()); p.Total != 1 {
		t.Errorf("成功筛选 = %d, 期望 1", p.Total)
	}
	if p, _ := svc.List(ctx, auditlog.Filter{Success: &no}, admin()); p.Total != 1 {
		t.Errorf("失败筛选 = %d, 期望 1", p.Total)
	}
	// 不传 = 不限。
	if p, _ := svc.List(ctx, auditlog.Filter{}, admin()); p.Total != 2 {
		t.Errorf("不筛选 = %d, 期望 2", p.Total)
	}
}

// TestTimeRangeFilter 覆盖时间范围。
func TestTimeRangeFilter(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	old := time.Now().Add(-48 * time.Hour)
	name := "old"
	db.Create(&model.AuditLog{
		At: old, OperatorID: ptr(alice), Source: "web",
		ResourceType: "vm", ResourceName: &name, Action: "vm.create", Success: true,
	})
	seed(t, db, alice, "vm.create", "vm", "recent", true)

	page, err := svc.List(ctx, auditlog.Filter{From: time.Now().Add(-1 * time.Hour)}, admin())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 || page.Items[0].ResourceName != "recent" {
		t.Errorf("时间范围筛选不对: %+v", page.Items)
	}
}

// TestOversizedPageIsTruncatedAndSaysSo 覆盖审计查询特有的失败模式。
//
// 普通列表少几条用户不会在意，而审计少了几条会让「这段时间没发生过这件事」
// 这个结论变成错的。因此宁可明确说「被截断了，请缩小范围」。
func TestOversizedPageIsTruncatedAndSaysSo(t *testing.T) {
	svc, _ := newTestEnv(t)

	page, err := svc.List(context.Background(), auditlog.Filter{PageSize: 100000}, admin())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !page.Truncated {
		t.Error("超过上限时应标记截断")
	}
	if page.Note == "" {
		t.Error("应给出可操作的说明")
	}
	if page.PageSize > 200 {
		t.Errorf("单页上限未生效: %d", page.PageSize)
	}
}

// TestFacetsRespectVisibility 覆盖筛选项本身的信息泄漏。
//
// 如果动作列表是全库的，一个租户就能通过它推断出系统里存在哪些功能被用过
// ——包括只属于管理员的那些。
func TestFacetsRespectVisibility(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seed(t, db, alice, "vm.create", "vm", "a", true)
	seed(t, db, bob, "node.maintenance.enter", "node", "n", true)

	tenantFacets, err := svc.Facets(ctx, tenant(alice))
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	joined := strings.Join(tenantFacets.Actions, ",")
	if strings.Contains(joined, "node.maintenance") {
		t.Errorf("租户不该看到别人的动作名: %s", joined)
	}
	if !strings.Contains(joined, "vm.create") {
		t.Errorf("应能看到自己的动作: %s", joined)
	}

	adminFacets, _ := svc.Facets(ctx, admin())
	if !strings.Contains(strings.Join(adminFacets.Actions, ","), "node.maintenance") {
		t.Error("管理员应能看到全部动作")
	}
}

func ptr(v int64) *int64 { return &v }

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
