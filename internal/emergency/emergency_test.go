package emergency_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/emergency"
	"k_cockpit/internal/model"
)

func newEnv(t *testing.T) (*gorm.DB, *emergency.Tool) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "emergency.db"),
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
	return db, emergency.New(db, audit.NewRecorder(db))
}

func seedUser(t *testing.T, db *gorm.DB, name string, mutate func(*model.User)) *model.User {
	t.Helper()
	hash := "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"
	email := name + "@example.com"
	verified := time.Now().Add(-time.Hour)
	secret := "encrypted-secret"
	codes := `["h1","h2"]`

	u := model.User{
		Username: name, PasswordHash: hash, Role: model.RoleAdmin,
		Status: model.UserStatusActive,
		Email:  &email, EmailVerifiedAt: &verified,
		TotpSecretEnc: &secret, TotpEnabled: true, RecoveryCodesHash: &codes,
	}
	if mutate != nil {
		mutate(&u)
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	return &u
}

// auditEntries 取该次操作写入的应急审计。
func auditEntries(t *testing.T, db *gorm.DB, action string) []model.AuditLog {
	t.Helper()
	var rows []model.AuditLog
	if err := db.Where("action = ?", action).Find(&rows).Error; err != nil {
		t.Fatalf("查询审计失败: %v", err)
	}
	return rows
}

// TestResetPasswordInvalidatesSessions 覆盖应急重置最关键的一处。
//
// 重置密码最常见的动机是「账号可能被盗」。如果只在数据库里换掉哈希而不动
// security_updated_at，攻击者手上那个已经建立的会话**继续有效**——这次
// 重置就只挡住了他不知道的凭据，而他已经拿到的那条路还开着。
//
// 这也是它与「用户自助改密码」在语义上的唯一差别：后者要求旧密码，
// 而这里不需要（旧密码正是丢掉的那样东西）。
func TestResetPasswordInvalidatesSessions(t *testing.T) {
	db, tool := newEnv(t)
	u := seedUser(t, db, "alice", nil)

	before := time.Now().Add(-time.Minute)
	if _, err := tool.ResetPassword(context.Background(), "alice", "a-very-long-temp-pw"); err != nil {
		t.Fatalf("重置失败: %v", err)
	}

	var after model.User
	if err := db.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if after.PasswordHash == u.PasswordHash {
		t.Error("密码哈希未改变")
	}
	if after.SecurityUpdatedAt == nil || !after.SecurityUpdatedAt.After(before) {
		t.Error("security_updated_at 未更新——按 R-010，全部既有会话应当立即失效")
	}
	// 应急重置出来的密码是临时的（可能经工单或口头传递），必须要求改掉。
	if !after.ForcePasswordChange {
		t.Error("应急重置后应要求下次登录修改密码")
	}
}

// TestResetPasswordWritesAudit 覆盖「必须自己写审计」。
//
// 这个工具绕过整个 Web 层，那条链路上所有的审计都不会发生。不在这里写的
// 话，「谁在什么时候重置了管理员密码」没有任何地方能回答——而那恰恰是事后
// 最需要回答的问题。
func TestResetPasswordWritesAudit(t *testing.T) {
	db, tool := newEnv(t)
	u := seedUser(t, db, "alice", nil)

	if _, err := tool.ResetPassword(context.Background(), "alice", "a-very-long-temp-pw"); err != nil {
		t.Fatalf("重置失败: %v", err)
	}

	rows := auditEntries(t, db, "emergency.password.reset")
	if len(rows) != 1 {
		t.Fatalf("应写入 1 条审计，实际 %d", len(rows))
	}
	e := rows[0]
	if e.Source != model.SourceEmergency {
		t.Errorf("来源 = %q, 期望 %q —— 事后要能区分「从面板点的」与「从命令行敲的」",
			e.Source, model.SourceEmergency)
	}
	if e.ResourceID == nil || *e.ResourceID != u.ID {
		t.Errorf("资源 ID = %v, 期望 %d", e.ResourceID, u.ID)
	}
	// 审计里不得出现凭据本身——写进去等于把它复制到了另一个地方。
	if e.Params != nil && containsAny(*e.Params, "a-very-long-temp-pw") {
		t.Error("审计参数里不得包含密码")
	}
}

// TestClear2FAClearsRecoveryCodesToo 覆盖清 2FA 的完整性。
//
// 只清 TOTP 而留下恢复码，等于把「另一把还能开的钥匙」留在抽屉里——
// 而用户以为两步验证已经被摘掉了。恢复码本身也是一种第二因子，只清一个
// 在安全上是自相矛盾的。
func TestClear2FAClearsRecoveryCodesToo(t *testing.T) {
	db, tool := newEnv(t)
	u := seedUser(t, db, "alice", nil)

	if _, err := tool.Clear2FA(context.Background(), "alice"); err != nil {
		t.Fatalf("清除失败: %v", err)
	}

	var after model.User
	if err := db.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if after.TotpSecretEnc != nil {
		t.Error("密码器密钥未清除")
	}
	if after.TotpEnabled {
		t.Error("两步验证开关未关闭")
	}
	if after.RecoveryCodesHash != nil {
		t.Error("恢复码未清除——只摘掉密码器会留下另一把还能开的钥匙")
	}
	if after.SecurityUpdatedAt == nil {
		t.Error("安全信息变更应更新 security_updated_at（R-010）")
	}
	if len(auditEntries(t, db, "emergency.2fa.clear")) != 1 {
		t.Error("应写入 1 条审计")
	}
}

// TestClearEmailClearsVerifiedFlag 覆盖邮箱清除的完整性。
//
// 留着一个「已验证」的时间戳而邮箱是空的，会让后续任何「按已验证邮箱查找」
// 的逻辑拿到一个自相矛盾的状态。
func TestClearEmailClearsVerifiedFlag(t *testing.T) {
	db, tool := newEnv(t)
	u := seedUser(t, db, "alice", nil)

	if _, err := tool.ClearEmail(context.Background(), "alice"); err != nil {
		t.Fatalf("清除失败: %v", err)
	}

	var after model.User
	if err := db.First(&after, u.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if after.Email != nil {
		t.Error("邮箱未清除")
	}
	if after.EmailVerifiedAt != nil {
		t.Error("验证状态未清除——留着一个已验证时间戳而邮箱为空是自相矛盾的状态")
	}
	// 记下原来绑的是什么：追查「这个账号什么时候被改绑过」时旧地址是唯一线索。
	rows := auditEntries(t, db, "emergency.email.clear")
	if len(rows) != 1 {
		t.Fatalf("应写入 1 条审计，实际 %d", len(rows))
	}
	if rows[0].Params == nil || !containsAny(*rows[0].Params, "alice@example.com") {
		t.Error("审计里应记下原来的邮箱地址")
	}
}

// TestListAdminsReports2FAAndEmailState 覆盖 list-admins 的用途。
//
// 它是这个工具**最先要用到的**子命令：另外几个都要指定用户名，而在面板
// 不可用时，人未必记得住管理员叫什么。
func TestListAdminsReports2FAAndEmailState(t *testing.T) {
	db, tool := newEnv(t)
	seedUser(t, db, "alice", nil)
	// 一个未验证邮箱的管理员：地址填着，但「用邮箱找回」走不通——而这一点
	// 在被忽略时表现得很隐蔽。
	seedUser(t, db, "bob", func(u *model.User) { u.EmailVerifiedAt = nil })
	seedUser(t, db, "tenant1", func(u *model.User) {
		u.Role = model.RoleTenant
		u.TotpEnabled = false
	})

	admins, err := tool.ListAdmins(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(admins) != 2 {
		t.Fatalf("应只列出管理员，实际 %d 个: %+v", len(admins), admins)
	}
	byName := map[string]emergency.AdminView{}
	for _, a := range admins {
		byName[a.Username] = a
	}
	if !byName["alice"].Has2FA {
		t.Error("alice 应显示已启用两步验证")
	}
	if byName["bob"].EmailVerified {
		t.Error("bob 的邮箱未验证，应如实显示")
	}
}

// TestPasswordTooShortIsRejected 覆盖下限。
//
// 应急重置同样要过长度下限：一个临时设成 "123456" 的管理员账号，往往会在
// 重置之后一直留着——「临时」在现实里很少真的临时。
func TestPasswordTooShortIsRejected(t *testing.T) {
	db, tool := newEnv(t)
	// 用户要真实存在：实现里**先查用户再校验密码长度**，因为用户名不存在
	// 比密码太短更根本——对一个不存在的人设多长的密码都没有意义。
	seedUser(t, db, "alice", nil)

	_, err := tool.ResetPassword(context.Background(), "alice", "123")
	if err == nil {
		t.Fatal("过短的密码应被拒绝")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeInvalidParameter {
		t.Errorf("应是参数错误，实际 %v", err)
	}
}

// TestUnknownUserSaysHowToFindThem 覆盖找不到用户时的提示。
//
// 报错里带上「用 list-admins 查看」：在面板不可用的情况下，人未必记得住
// 管理员叫什么，而这时再去翻文档是最不该发生的事。
func TestUnknownUserSaysHowToFindThem(t *testing.T) {
	_, tool := newEnv(t)
	_, err := tool.ResetPassword(context.Background(), "nobody", "a-very-long-temp-pw")
	if err == nil {
		t.Fatal("不存在的用户应报错")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code != api.CodeResourceNotFound {
		t.Fatalf("应是「资源不存在」，实际 %v", err)
	}
	if !containsAny(apiErr.Message, "list-admins") {
		t.Errorf("报错应提示如何查找账号，实际 %q", apiErr.Message)
	}
}

// TestGeneratePasswordIsStrongEnough 覆盖临时密码的生成。
func TestGeneratePasswordIsStrongEnough(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		pw, err := emergency.GeneratePassword()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if len(pw) < 24 {
			t.Fatalf("临时密码过短: %d 位", len(pw))
		}
		if seen[pw] {
			t.Fatal("生成了重复的密码——随机源有问题")
		}
		seen[pw] = true
	}
}

func containsAny(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) &&
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()
}
