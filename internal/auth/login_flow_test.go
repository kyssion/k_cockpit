package auth_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// --- 替身 ---

// fakeMailer 记录"最近发出的一封信"，供测试取出验证码。
type fakeMailer struct {
	configured bool
	to         string
	scene      string
	code       string
	sent       int
}

func (f *fakeMailer) Configured(context.Context) (bool, error) { return f.configured, nil }

func (f *fakeMailer) SendVerificationCode(_ context.Context, to, scene, code string) error {
	f.to, f.scene, f.code = to, scene, code
	f.sent++
	return nil
}

// fakeVerifier 只认一个固定码。
type fakeVerifier struct{ ok bool }

func (f *fakeVerifier) VerifyLoginCode(_ context.Context, _ *model.User, code string) (bool, error) {
	return f.ok && code == "123456", nil
}

// newFlowService 构造带验证码表的认证服务。
func newFlowService(t *testing.T) (*auth.Service, *gorm.DB) {
	t.Helper()
	svc, db := newTestService(t)
	if err := db.AutoMigrate(&model.EmailVerification{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return svc, db
}

// --- 登录阶段 ---

func TestAdminFirstLoginStopsAtBootstrap(t *testing.T) {
	svc, db := newFlowService(t)
	createUser(t, db, "root", model.RoleAdmin, model.UserStatusActive)
	ctx := context.Background()

	first, err := svc.Login(ctx, "root", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if first.Stage != auth.StageBootstrap {
		t.Fatalf("管理员首次登录阶段 = %q, 期望 bootstrap_security", first.Stage)
	}

	// 中间态令牌不能当访问令牌用：否则二次验证与引导都成了装饰。
	if _, _, err := svc.AuthenticateStage(ctx, first.Token, model.TokenTypeAccess, client); err == nil {
		t.Error("中间态令牌被当作访问级会话接受")
	}

	skipped, err := svc.SkipBootstrap(ctx, first.Token, client)
	if err != nil {
		t.Fatalf("跳过引导失败: %v", err)
	}
	if skipped.Stage != auth.StageOK {
		t.Errorf("跳过后阶段 = %q, 期望 ok", skipped.Stage)
	}

	// 跳过后不再反复提示：一次跳过就是永久跳过，否则引导会变成每次登录
	// 都要点掉的弹窗。
	again, err := svc.Login(ctx, "root", password, client)
	if err != nil {
		t.Fatalf("再次登录失败: %v", err)
	}
	if again.Stage != auth.StageOK {
		t.Errorf("再次登录阶段 = %q, 期望 ok", again.Stage)
	}
}

func TestTOTPUserMustVerify(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	if err := db.Model(&model.User{}).Where("id = ?", id).
		Update("totp_enabled", true).Error; err != nil {
		t.Fatalf("启用 2FA 失败: %v", err)
	}
	verifier := &fakeVerifier{ok: true}
	svc.SetLoginVerifier(verifier)
	ctx := context.Background()

	res, err := svc.Login(ctx, "alice", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.Stage != auth.StageVerify {
		t.Fatalf("阶段 = %q, 期望 login_verify", res.Stage)
	}

	if _, err := svc.VerifyLogin(ctx, res.Token, "000000", client); err == nil {
		t.Error("错误验证码应当被拒绝")
	}

	// 中间态令牌一次性：换到访问会话后再用必须失败。
	done, err := svc.VerifyLogin(ctx, res.Token, "123456", client)
	if err != nil {
		t.Fatalf("二次验证失败: %v", err)
	}
	if done.Stage != auth.StageOK {
		t.Errorf("阶段 = %q, 期望 ok", done.Stage)
	}
	if _, err := svc.VerifyLogin(ctx, res.Token, "123456", client); err == nil {
		t.Error("中间态令牌应当只能用一次")
	}
}

func TestForcePasswordChangeBlocksUntilChanged(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "bob", model.RoleTenant, model.UserStatusActive)
	if err := db.Model(&model.User{}).Where("id = ?", id).
		Update("force_password_change", true).Error; err != nil {
		t.Fatalf("置强制改密失败: %v", err)
	}
	ctx := context.Background()

	res, err := svc.Login(ctx, "bob", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.Stage != auth.StageForceChange {
		t.Fatalf("阶段 = %q, 期望 force_password_change", res.Stage)
	}

	// 太短的密码必须被拒：初始密码本来就不属于使用者，放宽下限等于把
	// 弱口令固化下来。
	if _, err := svc.ForceChangePassword(ctx, res.Token, "short", client); err == nil {
		t.Error("过短的密码应当被拒绝")
	}

	newPassword := "a-brand-new-long-password"
	done, err := svc.ForceChangePassword(ctx, res.Token, newPassword, client)
	if err != nil {
		t.Fatalf("强制改密失败: %v", err)
	}
	if done.Stage != auth.StageOK {
		t.Errorf("阶段 = %q, 期望 ok", done.Stage)
	}

	var after model.User
	if err := db.Where("id = ?", id).First(&after).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if after.ForcePasswordChange {
		t.Error("改密后仍标记强制改密")
	}
	if _, err := svc.Login(ctx, "bob", password, client); err == nil {
		t.Error("旧密码仍可登录")
	}
	if _, err := svc.Login(ctx, "bob", newPassword, client); err != nil {
		t.Errorf("新密码无法登录: %v", err)
	}
}

// 跳过引导只是"不再提醒"，不是"永远视为未做安全设置"：后来补齐了邮箱与
// 两步验证，标记就应当被清掉。
func TestBootstrapCompleteClearsSkippedFlag(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "root", model.RoleAdmin, model.UserStatusActive)
	ctx := context.Background()

	first, err := svc.Login(ctx, "root", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if _, err := svc.SkipBootstrap(ctx, first.Token, client); err != nil {
		t.Fatalf("跳过引导失败: %v", err)
	}

	var user model.User
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	// 只绑邮箱、没开 2FA：条件不齐，标记不该被清掉。
	now := time.Now()
	user.Email = strPtrForTest("root@example.com")
	user.EmailVerifiedAt = &now
	if err := svc.CompleteBootstrap(ctx, &user); err == nil {
		t.Error("未启用两步验证就完成引导应当被拒绝")
	}

	user.TotpEnabled = true
	if err := svc.CompleteBootstrap(ctx, &user); err != nil {
		t.Fatalf("完成引导失败: %v", err)
	}

	var after model.User
	if err := db.Where("id = ?", id).First(&after).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if after.BootstrapSkipped {
		t.Error("补齐安全设置后仍标记已跳过")
	}
}

func strPtrForTest(v string) *string { return &v }

// 没配邮件时，邮箱相关的入口必须明确说"未配置"，而不是报 500——
// 邮件是可选能力，全新安装本来就没有 SMTP。
func TestEmailEndpointsDegradeWithoutMailer(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	var user model.User
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	ctx := context.Background()

	if err := svc.SendEmailCode(ctx, &user, "alice@example.com", client); err == nil {
		t.Error("未配置邮件时发送验证码应当失败")
	}
	if err := svc.RequestPasswordReset(ctx, "alice@example.com", client); err == nil {
		t.Error("未配置邮件时找回密码应当失败")
	}
}

// --- 邮箱与找回密码 ---

func TestEmailBindFlow(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	ctx := context.Background()
	mail := &fakeMailer{configured: true}
	svc.SetMailer(mail)

	var user model.User
	if err := db.Where("id = ?", id).First(&user).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if err := svc.SendEmailCode(ctx, &user, "Alice@Example.com", client); err != nil {
		t.Fatalf("发送验证码失败: %v", err)
	}
	if mail.sent != 1 || mail.code == "" {
		t.Fatalf("未发出验证码: %+v", mail)
	}
	// 规范化：邮箱大小写不该产生两个不同的身份。
	if mail.to != "alice@example.com" {
		t.Errorf("收件人 = %q, 期望小写形式", mail.to)
	}

	if err := svc.ConfirmEmail(ctx, &user, "alice@example.com", "000000"); err == nil {
		t.Error("错误验证码应当被拒绝")
	}
	if err := svc.ConfirmEmail(ctx, &user, "alice@example.com", mail.code); err != nil {
		t.Fatalf("确认绑定失败: %v", err)
	}

	var after model.User
	if err := db.Where("id = ?", id).First(&after).Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	if after.Email == nil || *after.Email != "alice@example.com" || after.EmailVerifiedAt == nil {
		t.Errorf("绑定结果不正确: email=%v verified=%v", after.Email, after.EmailVerifiedAt)
	}

	// 验证码一次性：绑完再用同一个码必须失败。
	if err := svc.ConfirmEmail(ctx, &after, "alice@example.com", mail.code); err == nil {
		t.Error("验证码应当只能用一次")
	}
}

func TestPasswordResetFlow(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	now := time.Now()
	if err := db.Model(&model.User{}).Where("id = ?", id).Updates(map[string]any{
		"email":             "alice@example.com",
		"email_verified_at": now,
	}).Error; err != nil {
		t.Fatalf("设置邮箱失败: %v", err)
	}
	ctx := context.Background()
	mail := &fakeMailer{configured: true}
	svc.SetMailer(mail)

	// 不存在的邮箱同样返回成功：否则这就是一个"谁注册了本面板"的查询接口。
	if err := svc.RequestPasswordReset(ctx, "nobody@example.com", client); err != nil {
		t.Errorf("未知邮箱应当返回成功: %v", err)
	}
	if mail.sent != 0 {
		t.Error("未知邮箱不应发信")
	}

	if err := svc.RequestPasswordReset(ctx, "alice@example.com", client); err != nil {
		t.Fatalf("发起找回密码失败: %v", err)
	}
	if mail.scene == "" || mail.code == "" {
		t.Fatal("未发出验证码")
	}

	if _, err := svc.VerifyResetCode(ctx, "alice@example.com", "000000", client); err == nil {
		t.Error("错误验证码应当被拒绝")
	}
	ticket, err := svc.VerifyResetCode(ctx, "alice@example.com", mail.code, client)
	if err != nil {
		t.Fatalf("校验验证码失败: %v", err)
	}
	if ticket == "" {
		t.Fatal("未返回重置票据")
	}

	// 弱密码同样要被拒：找回密码是重置，不是绕过密码策略的入口。
	if err := svc.ResetPassword(ctx, ticket, "123", client); err == nil {
		t.Error("过短的密码应当被拒绝")
	}

	newPassword := "reset-password-long-enough"
	if err := svc.ResetPassword(ctx, ticket, newPassword, client); err != nil {
		t.Fatalf("重置密码失败: %v", err)
	}
	if _, err := svc.Login(ctx, "alice", password, client); err == nil {
		t.Error("旧密码仍可登录")
	}
	if _, err := svc.Login(ctx, "alice", newPassword, client); err != nil {
		t.Errorf("新密码无法登录: %v", err)
	}
	// 票据用过后即失效。
	if err := svc.ResetPassword(ctx, ticket, "another-long-password", client); err == nil {
		t.Error("重置票据应当只能使用一次")
	}
}

// 未验证的邮箱不能用来找回密码：否则任何人都能给自己填一个别人的邮箱，
// 然后重置对方的账号。
func TestResetIgnoresUnverifiedEmail(t *testing.T) {
	svc, db := newFlowService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	if err := db.Model(&model.User{}).Where("id = ?", id).
		Update("email", "alice@example.com").Error; err != nil {
		t.Fatalf("设置邮箱失败: %v", err)
	}
	mail := &fakeMailer{configured: true}
	svc.SetMailer(mail)

	if err := svc.RequestPasswordReset(context.Background(), "alice@example.com", client); err != nil {
		t.Fatalf("发起找回密码失败: %v", err)
	}
	if mail.sent != 0 {
		t.Error("未验证的邮箱不应收到验证码")
	}
}
