package search_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/ops/search"
)

func newTestService(t *testing.T) (*search.Service, *gorm.DB) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "search.db"),
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
	if err := db.AutoMigrate(&model.VM{}, &model.Template{}, &model.Node{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return search.NewService(db), db
}

func ptr[T any](v T) *T { return &v }

func addVM(t *testing.T, db *gorm.DB, name string, owner int64) {
	t.Helper()
	row := model.VM{
		NodeID: 1, Name: name, OwnerID: ptr(owner),
		Status: model.VMStatusStopped, Present: true, VCPU: 1, MemoryMB: 512,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("插入 VM %s 失败: %v", name, err)
	}
}

func addTemplate(t *testing.T, db *gorm.DB, name string, creator int64, published bool) {
	t.Helper()
	row := model.Template{
		NodeID: 1, Name: name, CreatedBy: ptr(creator), Published: published,
		Status: model.TemplateReady, DiskPath: ptr("/tmp/" + name + ".qcow2"),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("插入模板 %s 失败: %v", name, err)
	}
}

func addNode(t *testing.T, db *gorm.DB, name string) {
	t.Helper()
	row := model.Node{Name: name, Status: model.NodeStatusOnline}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("插入节点 %s 失败: %v", name, err)
	}
}

func namesOf(matches []search.Match, kind string) []string {
	out := []string{}
	for _, m := range matches {
		if m.Kind == kind {
			out = append(out, m.Name)
		}
	}
	return out
}

// 空串与单字符不检索：直接返回空，不碰数据库。
func TestSearchRejectsEmptyAndSingleRuneQueries(t *testing.T) {
	svc, db := newTestService(t)
	addVM(t, db, "web-01", 1)
	v := authz.Viewer{UserID: 1, IsAdmin: true}
	for _, q := range []string{"", "   ", "w", "中"} {
		got, err := svc.Search(context.Background(), q, v)
		if err != nil {
			t.Fatalf("q=%q 返回错误: %v", q, err)
		}
		if len(got) != 0 {
			t.Errorf("q=%q 应返回空结果，实际 %d 条", q, len(got))
		}
	}
}

// 通配符必须按字面量匹配：搜 "%" 不能命中所有名字。
func TestSearchEscapesLikeWildcards(t *testing.T) {
	svc, db := newTestService(t)
	addVM(t, db, "web-01", 1)
	addVM(t, db, "web-02", 1)
	addVM(t, db, "100%-cpu", 1)
	addVM(t, db, "a_b_test", 1)
	v := authz.Viewer{UserID: 1, IsAdmin: true}

	got, err := svc.Search(context.Background(), "%", v)
	if err != nil {
		t.Fatal(err)
	}
	// "%" 是单字符，被最短长度规则挡住；用两字符 "%x" 才能进到 LIKE。
	if len(got) != 0 {
		t.Errorf("单字符 %% 应被最短长度挡住，实际 %d 条", len(got))
	}

	got, err = svc.Search(context.Background(), "%-", v)
	if err != nil {
		t.Fatal(err)
	}
	gotNames := namesOf(got, search.KindVM)
	// "100%-cpu" 里字面包含 "%-"，应当命中；web-01/web-02 不含，不能被
	// 未转义的 % 当通配符捞出来。
	if len(gotNames) != 1 || gotNames[0] != "100%-cpu" {
		t.Errorf("搜 %%- 应只按字面命中 100%%-cpu，实际 %v", gotNames)
	}

	got, err = svc.Search(context.Background(), "100%", v)
	if err != nil {
		t.Fatal(err)
	}
	if gotNames = namesOf(got, search.KindVM); len(gotNames) != 1 || gotNames[0] != "100%-cpu" {
		t.Errorf("搜 100%% 应只命中 100%%-cpu，实际 %v", gotNames)
	}

	got, err = svc.Search(context.Background(), "a_b", v)
	if err != nil {
		t.Fatal(err)
	}
	if gotNames = namesOf(got, search.KindVM); len(gotNames) != 1 || gotNames[0] != "a_b_test" {
		t.Errorf("搜 a_b 应只命中 a_b_test（_ 不是通配符），实际 %v", gotNames)
	}
}

// 租户不能通过搜索看到别人的虚拟机。
//
// 这条守住的是一个容易漏掉的越权：匹配条件里含 OR，若与 owner_id 的
// AND 没加括号，SQL 优先级会让 owner 过滤只作用在最后一项上。
func TestSearchTenantCannotSeeOtherOwnersVMs(t *testing.T) {
	svc, db := newTestService(t)
	addVM(t, db, "web-mine", 1)
	addVM(t, db, "web-others", 2)
	addVM(t, db, "db-mine", 1)
	v := authz.Viewer{UserID: 1, IsAdmin: false}

	got, err := svc.Search(context.Background(), "web", v)
	if err != nil {
		t.Fatal(err)
	}
	gotNames := namesOf(got, search.KindVM)
	if len(gotNames) != 1 || gotNames[0] != "web-mine" {
		t.Errorf("租户 1 搜 web 只应看到自己的 web-mine，实际 %v", gotNames)
	}

	// 备注/IP 命中同样要受归属约束（OR 分支最容易漏）。
	if err := db.Model(&model.VM{}).Where("name = ?", "db-mine").
		Update("remark", "web-backend").Error; err != nil {
		t.Fatal(err)
	}
	got, err = svc.Search(context.Background(), "web-backend", v)
	if err != nil {
		t.Fatal(err)
	}
	if gotNames = namesOf(got, search.KindVM); len(gotNames) != 1 || gotNames[0] != "db-mine" {
		t.Errorf("备注命中也应只返回自己的，实际 %v", gotNames)
	}
}

// 租户看得到已发布模板与自己的私有模板，看不到别人的私有模板。
func TestSearchTenantTemplateVisibility(t *testing.T) {
	svc, db := newTestService(t)
	addTemplate(t, db, "tpl-published", 2, true)
	addTemplate(t, db, "tpl-private-other", 2, false)
	addTemplate(t, db, "tpl-private-mine", 1, false)
	v := authz.Viewer{UserID: 1, IsAdmin: false}

	got, err := svc.Search(context.Background(), "tpl", v)
	if err != nil {
		t.Fatal(err)
	}
	gotNames := namesOf(got, search.KindTemplate)
	if len(gotNames) != 2 {
		t.Errorf("租户应看到 2 个模板（已发布 + 自己的私有），实际 %v", gotNames)
	}
	for _, n := range gotNames {
		if n == "tpl-private-other" {
			t.Errorf("租户看到了别人的私有模板：%v", gotNames)
		}
	}
}

// 节点是平台级资源，只对管理员开放。
func TestSearchNodesAreAdminOnly(t *testing.T) {
	svc, db := newTestService(t)
	addNode(t, db, "kvm-node-1")
	addVM(t, db, "kvm-vm-1", 1)

	tenant, err := svc.Search(context.Background(), "kvm", authz.Viewer{UserID: 1, IsAdmin: false})
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(tenant, search.KindNode); len(got) != 0 {
		t.Errorf("租户不应搜到节点，实际 %v", got)
	}
	admin, err := svc.Search(context.Background(), "kvm", authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := namesOf(admin, search.KindNode); len(got) != 1 || got[0] != "kvm-node-1" {
		t.Errorf("管理员应搜到节点，实际 %v", got)
	}
}

// 每一类最多 5 条，避免搜索面板变成列表页。
func TestSearchCapsResultsPerKind(t *testing.T) {
	svc, db := newTestService(t)
	for i := 0; i < 9; i++ {
		addVM(t, db, fmt.Sprintf("cap-vm-%02d", i), 1)
	}
	got, err := svc.Search(context.Background(), "cap-vm", authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(namesOf(got, search.KindVM)); n != 5 {
		t.Errorf("VM 结果应被限制在 5 条，实际 %d", n)
	}
}
