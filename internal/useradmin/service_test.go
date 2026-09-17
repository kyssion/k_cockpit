package useradmin_test

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
	"k_cockpit/internal/useradmin"
)

func newTestEnv(t *testing.T) (*useradmin.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "user.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.Session{}, &model.VM{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return useradmin.NewService(db, audit.NewRecorder(db), nil), db
}

// admin 用一个**不与自增主键冲突**的 id。
//
// 之前用的是 1，而测试库里的自增从 1 开始——于是第一个被建出来的用户
// 恰好就是「操作者自己」，所有针对它的操作都被「不能封禁自己」挡住了。
// 那几条用例因此失败在一个与被测行为无关的地方。
func admin() authz.Viewer { return authz.Viewer{UserID: 9999, IsAdmin: true} }

// seedUser 直接写一条用户记录。
func seedUser(t *testing.T, db *gorm.DB, name, role, status string) *model.User {
	t.Helper()
	row := model.User{Username: name, PasswordHash: "x", Role: role, Status: status}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
	return &row
}

// TestCreatedUserIsPendingAndMustChangePassword 覆盖新建账号的两个默认值。
//
// 「待激活」给了一个「先建好、确认无误、再让人用」的中间状态；而强制改密
// 让管理员给的初始密码不会长期留在那里——那正是初始密码最主要的风险。
func TestCreatedUserIsPendingAndMustChangePassword(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	view, err := svc.Create(ctx, useradmin.CreateRequest{
		Username: "alice", Password: "initial-pass",
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if view.Status != model.UserStatusPending {
		t.Errorf("状态 = %q, 期望 pending", view.Status)
	}

	var row model.User
	db.Where("username = ?", "alice").First(&row)
	if !row.ForcePasswordChange {
		t.Error("应要求首次登录改密")
	}
	if row.PasswordHash == "initial-pass" {
		t.Fatal("密码被明文存储了")
	}
}

// TestAuditNeverRecordsPassword 覆盖一处会让审计流水变成后门的问题。
//
// 审计流水会被很多人看到，而一条初始密码在里面就等于一个长期可用的后门。
func TestAuditNeverRecordsPassword(t *testing.T) {
	svc, db := newTestEnv(t)

	if _, err := svc.Create(context.Background(), useradmin.CreateRequest{
		Username: "bob", Password: "super-secret-pw",
	}, admin(), "root", ""); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	var rows []model.AuditLog
	db.Find(&rows)
	for _, r := range rows {
		for _, p := range []*string{r.Params, r.BeforeState, r.AfterState, r.Error} {
			if p != nil && strings.Contains(*p, "super-secret-pw") {
				t.Fatal("审计流水里出现了密码——它会变成一个长期可用的后门")
			}
		}
	}
}

func TestDuplicateUsernameRejected(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, useradmin.CreateRequest{
		Username: "dup", Password: "password123",
	}, admin(), "root", ""); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	_, err := svc.Create(ctx, useradmin.CreateRequest{
		Username: "dup", Password: "password123",
	}, admin(), "root", "")
	assertStatus(t, err, 409)
}

func TestShortPasswordRejected(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.Create(context.Background(), useradmin.CreateRequest{
		Username: "short", Password: "1234",
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

// TestBanCascadeWarningsDoNotFailTheOperation 覆盖本包最核心的语义。
//
// 封禁的实质是「这个人不能再用系统」，而**置状态那一步就达成了它**；
// 后面两步（撤销会话、停运行中的虚拟机）是减少暴露面的加固。
//
// 让加固的失败把整个操作判为失败，会让界面显示「封禁失败」——而那人其实
// 已经被封了。那比只做到一半更糟。
func TestBanCascadeWarningsDoNotFailTheOperation(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	victim := seedUser(t, db, "victim", model.RoleTenant, model.UserStatusActive)
	// 造一台运行中的虚拟机，让级联的第三步有东西可做。
	owner := victim.ID
	db.Create(&model.VM{
		NodeID: 1, Name: "vm-1", Status: model.VMStatusRunning,
		OwnerID: &owner, Present: true,
	})

	result, err := svc.SetStatus(ctx, victim.ID, model.UserStatusBanned, admin(), "root", "")
	if err != nil {
		t.Fatalf("封禁不该因为级联动作而出错: %v", err)
	}
	if result.User.Status != model.UserStatusBanned {
		t.Error("状态未更新")
	}
	// 级联的第三步会产生一条说明（它是有意不做下发的，见 service 注释）。
	if len(result.Warnings) == 0 {
		t.Error("级联动作的结果应如实告知——包括「标记为停止但未下发」这件事")
	}

	// 虚拟机被标记为停止，但**没有真的下发**。
	var vm model.VM
	db.Where("name = ?", "vm-1").First(&vm)
	if vm.Status != model.VMStatusStopped {
		t.Error("运行中的虚拟机应被标记为停止")
	}
}

// TestBanRevokesSessions 覆盖级联的第二步。
func TestBanRevokesSessions(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	victim := seedUser(t, db, "sess", model.RoleTenant, model.UserStatusActive)
	db.Create(&model.Session{UserID: victim.ID, SessionID: "s1"})

	if _, err := svc.SetStatus(ctx, victim.ID, model.UserStatusBanned, admin(), "root", ""); err != nil {
		t.Fatalf("封禁失败: %v", err)
	}

	var n int64
	db.Model(&model.Session{}).Where("user_id = ?", victim.ID).Count(&n)
	if n != 0 {
		t.Error("封禁应撤销该用户的全部会话——否则他手上的会话仍然有效")
	}
}

// TestBanIsIdempotent 覆盖重复封禁。
func TestBanIsIdempotent(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	victim := seedUser(t, db, "idem", model.RoleTenant, model.UserStatusActive)
	for i := 0; i < 3; i++ {
		if _, err := svc.SetStatus(ctx, victim.ID, model.UserStatusBanned, admin(), "root", ""); err != nil {
			t.Fatalf("第 %d 次封禁失败: %v", i+1, err)
		}
	}
}

// --- 三处不可逆的自锁 ---

// TestCannotBanSelf 覆盖一处会让自己再也进不去的操作。
func TestCannotBanSelf(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	me := seedUser(t, db, "me", model.RoleAdmin, model.UserStatusActive)
	meViewer := authz.Viewer{UserID: me.ID, IsAdmin: true}

	_, err := svc.SetStatus(ctx, me.ID, model.UserStatusBanned, meViewer, "me", "")
	assertStatus(t, err, 422)
}

func TestCannotChangeOwnRole(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	me := seedUser(t, db, "me2", model.RoleAdmin, model.UserStatusActive)
	meViewer := authz.Viewer{UserID: me.ID, IsAdmin: true}

	_, err := svc.Update(ctx, me.ID, useradmin.UpdateRequest{Role: model.RoleTenant}, meViewer, "me", "")
	assertStatus(t, err, 422)
}

// TestCannotRemoveLastAdmin 覆盖**不可逆的自锁**：之后没人能进管理界面，
// 只能上宿主机改数据库。
func TestCannotRemoveLastAdmin(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	// 只有这一个可用管理员。
	only := seedUser(t, db, "only-admin", model.RoleAdmin, model.UserStatusActive)

	// 目录：找一个**别的**管理员来操作，但他自己也是被保护的对象。
	other := seedUser(t, db, "other-admin", model.RoleAdmin, model.UserStatusActive)
	otherViewer := authz.Viewer{UserID: other.ID, IsAdmin: true}

	// 把 other 降级之后，only 是最后一个 —— 但这次降级本身允许。
	if _, err := svc.Update(ctx, other.ID, useradmin.UpdateRequest{Role: model.RoleTenant},
		otherViewer, "other", ""); err == nil {
		t.Fatal("不该允许降级最后两个管理员中的任一个（它们互为最后的备份）")
	}

	// 显式验证：把 other 封掉，只剩 only，此时动 only 必须被拒。
	if _, err := svc.SetStatus(ctx, other.ID, model.UserStatusBanned, admin(), "root", ""); err != nil {
		t.Fatalf("封禁 other 失败: %v", err)
	}
	_, err := svc.SetStatus(ctx, only.ID, model.UserStatusBanned, admin(), "root", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "最后一个") {
		t.Errorf("拒绝文案应说明后果: %v", err)
	}
}

// TestDeleteRejectsUserWithVMs 覆盖一处会留下「无人管理的资源」的删除。
func TestDeleteRejectsUserWithVMs(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	owner := seedUser(t, db, "has-vm", model.RoleTenant, model.UserStatusActive)
	oid := owner.ID
	db.Create(&model.VM{NodeID: 1, Name: "vm-x", Status: model.VMStatusStopped, OwnerID: &oid, Present: true})

	err := svc.Delete(ctx, owner.ID, admin(), "root", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "虚拟机") {
		t.Errorf("拒绝文案应说明原因与数量: %v", err)
	}

	// 没有虚拟机的用户要能删——只测前者拦不住一个「全都删不掉」的实现。
	free := seedUser(t, db, "no-vm", model.RoleTenant, model.UserStatusActive)
	if err := svc.Delete(ctx, free.ID, admin(), "root", ""); err != nil {
		t.Errorf("没有虚拟机的用户应可删除: %v", err)
	}
}

// TestDeleteIsSoft 覆盖软删除。
//
// 硬删除会让审计流水里的 operator_id 指向一个不存在的用户，
// 那些记录就再也解释不清了。
func TestDeleteIsSoft(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	u := seedUser(t, db, "soft", model.RoleTenant, model.UserStatusActive)
	if err := svc.Delete(ctx, u.ID, admin(), "root", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// 记录还在（只是带上了 deleted_at）。
	var n int64
	db.Unscoped().Model(&model.User{}).Where("id = ?", u.ID).Count(&n)
	if n != 1 {
		t.Error("应是软删除——硬删除会让审计记录失去可解释性")
	}

	// 列表里看不到。
	page, _ := svc.List(ctx, useradmin.ListFilter{})
	for _, item := range page.Items {
		if item.ID == u.ID {
			t.Error("已删除的用户不该出现在列表里")
		}
	}
}

func TestListFilters(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	seedUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	seedUser(t, db, "bob", model.RoleAdmin, model.UserStatusBanned)

	page, err := svc.List(ctx, useradmin.ListFilter{Keyword: "ali"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != 1 || page.Items[0].Username != "alice" {
		t.Errorf("关键字筛选不对: %+v", page.Items)
	}

	page, _ = svc.List(ctx, useradmin.ListFilter{Status: model.UserStatusBanned})
	if page.Total != 1 || page.Items[0].Username != "bob" {
		t.Errorf("状态筛选不对: %+v", page.Items)
	}

	page, _ = svc.List(ctx, useradmin.ListFilter{Role: model.RoleAdmin})
	if page.Total != 1 {
		t.Errorf("角色筛选不对: %d", page.Total)
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
