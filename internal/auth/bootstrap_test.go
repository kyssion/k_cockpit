package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

const adminPassword = "Str0ng-Passw0rd-XYZ"

func newBootstrapDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "bootstrap.db"),
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
	if err := db.AutoMigrate(&model.User{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

func TestBootstrapIssuesTokenWhenEmpty(t *testing.T) {
	db := newBootstrapDB(t)

	_, token, err := auth.NewBootstrap(db, audit.NewRecorder(db))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if len(token) != 48 {
		t.Errorf("令牌长度 = %d, 期望 48（24 字节 hex）", len(token))
	}
}

// 已有管理员时不得再生成令牌——否则等于每次重启都给出一条初始化后门。
func TestBootstrapSkipsTokenWhenInitialized(t *testing.T) {
	db := newBootstrapDB(t)
	createUser(t, db, "alice", model.RoleAdmin, model.UserStatusActive)

	_, token, err := auth.NewBootstrap(db, audit.NewRecorder(db))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if token != "" {
		t.Errorf("系统已初始化但仍生成了令牌: %s", token)
	}
}

// 只有租户、没有管理员时，系统仍属未初始化。
func TestBootstrapIgnoresTenantOnly(t *testing.T) {
	db := newBootstrapDB(t)
	createUser(t, db, "bob", model.RoleTenant, model.UserStatusActive)

	_, token, err := auth.NewBootstrap(db, audit.NewRecorder(db))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if token == "" {
		t.Error("仅有租户时仍应视为未初始化")
	}
}

// 被软删除的管理员不计入——否则删除唯一管理员后系统将永久无法初始化。
func TestBootstrapIgnoresSoftDeletedAdmin(t *testing.T) {
	db := newBootstrapDB(t)
	createUser(t, db, "alice", model.RoleAdmin, model.UserStatusActive)
	db.Model(&model.User{}).Where("username = ?", "alice").Update("deleted_at", "2026-01-01 00:00:00")

	_, token, err := auth.NewBootstrap(db, audit.NewRecorder(db))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if token == "" {
		t.Error("管理员已被软删除时应视为未初始化")
	}
}

func TestCreateAdminSucceeds(t *testing.T) {
	db := newBootstrapDB(t)
	bootstrap, token, err := auth.NewBootstrap(db, audit.NewRecorder(db))
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	user, err := bootstrap.CreateAdmin(context.Background(), token, "admin", adminPassword, "10.0.0.1")
	if err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	if user.Role != model.RoleAdmin || user.Status != model.UserStatusActive {
		t.Errorf("创建的用户 = %+v, 期望 admin 且 active", user)
	}

	// 密码必须以哈希存储，且能通过校验。
	if user.PasswordHash == adminPassword {
		t.Fatal("密码被明文存储")
	}
	ok, err := auth.VerifyPassword(user.PasswordHash, adminPassword)
	if err != nil || !ok {
		t.Errorf("存储的哈希无法校验原密码: ok=%v err=%v", ok, err)
	}

	// 创建后系统应报告已初始化。
	initialized, err := bootstrap.Initialized(context.Background())
	if err != nil || !initialized {
		t.Errorf("创建后 Initialized = %v, err=%v", initialized, err)
	}
}

// 令牌是一次性的：创建成功后同一令牌不可再用。
func TestCreateAdminTokenIsSingleUse(t *testing.T) {
	db := newBootstrapDB(t)
	bootstrap, token, _ := auth.NewBootstrap(db, audit.NewRecorder(db))

	if _, err := bootstrap.CreateAdmin(context.Background(), token, "admin", adminPassword, "10.0.0.1"); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}

	_, err := bootstrap.CreateAdmin(context.Background(), token, "admin2", adminPassword, "10.0.0.1")
	if err == nil {
		t.Fatal("同一令牌被重复使用")
	}
}

func TestCreateAdminRejectsWrongToken(t *testing.T) {
	db := newBootstrapDB(t)
	bootstrap, token, _ := auth.NewBootstrap(db, audit.NewRecorder(db))

	cases := map[string]string{
		"错误令牌": strings.Repeat("f", len(token)),
		"空令牌":  "",
		"前缀正确": token[:len(token)-1],
	}

	for name, bad := range cases {
		_, err := bootstrap.CreateAdmin(context.Background(), bad, "admin", adminPassword, "10.0.0.1")
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != 403 {
			t.Errorf("%s: 期望 403, 实际 %v", name, err)
		}
	}

	// 失败的尝试不得创建任何用户。
	var count int64
	db.Model(&model.User{}).Count(&count)
	if count != 0 {
		t.Errorf("令牌校验失败却创建了 %d 个用户", count)
	}
}

// 系统已初始化时，初始化接口必须无条件拒绝（不能只靠前端隐藏）。
func TestCreateAdminRejectsWhenInitialized(t *testing.T) {
	db := newBootstrapDB(t)
	createUser(t, db, "existing", model.RoleAdmin, model.UserStatusActive)

	bootstrap, token, _ := auth.NewBootstrap(db, audit.NewRecorder(db))
	if token != "" {
		t.Fatal("已初始化却拿到令牌")
	}

	_, err := bootstrap.CreateAdmin(context.Background(), token, "admin", adminPassword, "10.0.0.1")
	if err == nil {
		t.Fatal("已初始化仍允许创建管理员")
	}

	var count int64
	db.Model(&model.User{}).Where("role = ?", model.RoleAdmin).Count(&count)
	if count != 1 {
		t.Errorf("管理员数量 = %d, 期望仍为 1", count)
	}
}

func TestCreateAdminValidatesInput(t *testing.T) {
	db := newBootstrapDB(t)
	bootstrap, token, _ := auth.NewBootstrap(db, audit.NewRecorder(db))

	cases := map[string]struct{ username, password string }{
		"用户名过短":    {"ab", adminPassword},
		"用户名含非法字符": {"ad min!", adminPassword},
		"用户名过长":    {strings.Repeat("a", 65), adminPassword},
		"密码过短":     {"admin", "short"},
		"密码包含用户名":  {"adminuser", "adminuser-12345"},
	}

	for name, tc := range cases {
		_, err := bootstrap.CreateAdmin(context.Background(), token, tc.username, tc.password, "10.0.0.1")
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Errorf("%s: 期望 400, 实际 %v", name, err)
		}
	}
}

// 两个并发请求都拿到令牌时，只能有一个成功——否则会创建出两个管理员。
func TestCreateAdminIsConcurrencySafe(t *testing.T) {
	db := newBootstrapDB(t)
	// SQLite 单连接：并发写会串行化，这里验证的是应用层的互斥而非数据库行为。
	bootstrap, token, _ := auth.NewBootstrap(db, audit.NewRecorder(db))

	const attempts = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "admin" + string(rune('a'+i))
			if _, err := bootstrap.CreateAdmin(context.Background(), token, name, adminPassword, "10.0.0.1"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if successes != 1 {
		t.Errorf("并发创建成功次数 = %d, 期望恰好 1", successes)
	}

	var count int64
	db.Model(&model.User{}).Where("role = ?", model.RoleAdmin).Count(&count)
	if count != 1 {
		t.Errorf("管理员数量 = %d, 期望 1", count)
	}
}

// 初始化是安全事件，成功与失败都必须留审计，且审计中不得出现令牌。
func TestCreateAdminIsAudited(t *testing.T) {
	db := newBootstrapDB(t)
	bootstrap, token, _ := auth.NewBootstrap(db, audit.NewRecorder(db))

	_, _ = bootstrap.CreateAdmin(context.Background(), "wrong-token", "admin", adminPassword, "10.0.0.1")
	_, _ = bootstrap.CreateAdmin(context.Background(), token, "admin", adminPassword, "10.0.0.1")

	var logs []model.AuditLog
	db.Where("action = ?", "system.bootstrap_admin").Order("id").Find(&logs)
	if len(logs) != 2 {
		t.Fatalf("审计记录数 = %d, 期望 2", len(logs))
	}
	if logs[0].Success {
		t.Error("失败的初始化被记为成功")
	}
	if !logs[1].Success {
		t.Error("成功的初始化被记为失败")
	}
	if logs[0].ClientIP == nil || *logs[0].ClientIP != "10.0.0.1" {
		t.Error("审计未记录来源 IP")
	}

	for _, l := range logs {
		if (l.Params != nil && strings.Contains(*l.Params, token)) ||
			(l.Error != nil && strings.Contains(*l.Error, token)) ||
			(l.ResourceName != nil && strings.Contains(*l.ResourceName, token)) {
			t.Error("审计中出现了初始化令牌")
		}
	}
}
