package settings_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/cryptoutil"
	"k_cockpit/internal/service/platform/settings"
)

// 本文件是 U-33 的证据。
//
// 复核时实测到的两件事（r22）：
//  1. `system_setting.value` 里 `notification.smtp_password` 是**明文**，
//     `select value from system_setting` 原样取回；
//  2. 再改一次口令后 `previous_value` 里躺着**上一手明文**，也就是说轮换
//     口令并不会让旧口令离开数据库。
//
// 这两条都不是「接口回显」问题（R-009 已经挡住了），而是静态存储问题：
// 拿到库、拿到备份、拿到一个只读副本，就等于拿到邮局口令——而运维的 SMTP
// 口令通常与邮箱是同一个，泄漏会溢出本项目。
//
// 修法：接入 cryptoutil（AES-GCM，TOTP/VNC/来宾凭据已在用），敏感项的
// 写入加密、读取解密；敏感项不再保留前值；启动时一次性迁移存量明文。

// secretAtRest 直接读库，返回原始存储值。
func secretAtRest(t *testing.T, db *gorm.DB, key string) string {
	t.Helper()
	var row model.SystemSetting
	if err := db.Where("key = ?", key).First(&row).Error; err != nil {
		t.Fatalf("读取 %s 失败: %v", key, err)
	}
	if row.Value == nil {
		return ""
	}
	return *row.Value
}

// TestSecretIsEncryptedAtRest 是最核心的一条：写进去的口令在库里不可读。
func TestSecretIsEncryptedAtRest(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	const password = "KC-AtRest-Probe-9f3a"
	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_password": password,
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("保存口令失败: %v", err)
	}

	raw := secretAtRest(t, db, "notification.smtp_password")
	if raw == password {
		t.Fatal("库里是明文口令 —— 静态加密没有生效")
	}
	if strings.Contains(raw, password) {
		t.Fatalf("库里含明文片段: %q", raw)
	}

	// 服务端自己必须还能拿到明文——否则邮件就发不出去了。
	cfg, err := svc.MailConfig(ctx)
	if err != nil {
		t.Fatalf("读取邮件配置失败: %v", err)
	}
	if cfg.Password != password {
		t.Errorf("解出的口令 = %q, 期望 %q", cfg.Password, password)
	}

	// 对外接口仍然不回传明文（R-009），只告诉界面「已设置」。
	item, err := svc.Get(ctx, "notification.smtp_password")
	if err != nil {
		t.Fatalf("读取设置项失败: %v", err)
	}
	if item.Value != "" {
		t.Errorf("设置项回传了明文: %q", item.Value)
	}
	if !item.IsSet {
		t.Error("应标记 is_set = true")
	}
}

// TestSecretRotationLeavesNoPreviousValue 覆盖「换口令不让旧口令留在库里」。
//
// 这是 U-33 的第二半。previous_value 的用途是回滚错改，而回滚一个已经
// 失效的口令没有意义；代价却是上一手明文永久留在库里，一次泄漏变成两次。
func TestSecretRotationLeavesNoPreviousValue(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	for _, pw := range []string{"KC-Rotate-A-1111", "KC-Rotate-B-2222"} {
		if _, err := svc.Update(ctx, map[string]string{
			"notification.smtp_password": pw,
		}, 1, "admin", "10.0.0.1"); err != nil {
			t.Fatalf("保存 %s 失败: %v", pw, err)
		}
	}

	var row model.SystemSetting
	if err := db.Where("key = ?", "notification.smtp_password").First(&row).Error; err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if row.PreviousValue != nil {
		t.Errorf("轮换后仍保留了前值: %q（旧口令不该留在库里）", *row.PreviousValue)
	}
	// 当前值也不能含任何一手的明文。
	if row.Value == nil || strings.Contains(*row.Value, "KC-Rotate") {
		t.Errorf("当前值含明文片段: %v", row.Value)
	}
	// 解出来是最新那一手。
	if cfg, _ := svc.MailConfig(ctx); cfg.Password != "KC-Rotate-B-2222" {
		t.Errorf("生效口令 = %q, 期望最新值", cfg.Password)
	}
}

// TestSecretRollbackRejected 覆盖敏感项不可回滚。
//
// 即便库里残留了某种前值，接口也必须拒绝——否则会有人把一次泄漏的口令
// 又「恢复」回来。
func TestSecretRollbackRejected(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_password": "first",
	}, 1, "admin", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 直接往库里塞一个前值，模拟升级前遗留的数据。
	prev := "legacy-plaintext"
	if err := db.Model(&model.SystemSetting{}).
		Where("key = ?", "notification.smtp_password").
		Update("previous_value", prev).Error; err != nil {
		t.Fatalf("写前值失败: %v", err)
	}

	_, err := svc.Rollback(ctx, "notification.smtp_password", 1, "admin", "")
	if err == nil {
		t.Fatal("敏感项不该支持回滚")
	}
	var apiErr *api.Error
	if ok := asAPIError(err, &apiErr); !ok {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != 422 {
		t.Errorf("状态码 = %d, 期望 422", apiErr.Status)
	}
	// spec 上也要声明成不可回滚，否则界面会画一个点了就报错的按钮。
	item, _ := svc.Get(ctx, "notification.smtp_password")
	if item.Rollbackable {
		t.Error("spec.Rollbackable 应为 false")
	}
	if item.CanRollback {
		t.Error("不应标记 can_rollback")
	}
}

// TestEncryptLegacySecretsMigratesPlaintext 覆盖存量明文的启动迁移。
//
// 升级路径上一定有明文行——不迁的话「加密上线」只对新写入生效，而那条
// 老口令可以原样躺到下一次修改，中间可能是几个月。
func TestEncryptLegacySecretsMigratesPlaintext(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	// 直接写一行明文，模拟加密上线前留下的数据。
	plain := "Legacy-Plain-SMTP-Pass"
	prevPlain := "Even-Older-Pass"
	if err := db.Create(&model.SystemSetting{
		Key: "notification.smtp_password", Value: &plain, PreviousValue: &prevPlain,
	}).Error; err != nil {
		t.Fatalf("写入明文行失败: %v", err)
	}

	n, err := svc.EncryptLegacySecrets(ctx)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if n != 1 {
		t.Errorf("迁移条数 = %d, 期望 1", n)
	}

	var row model.SystemSetting
	if err := db.Where("key = ?", "notification.smtp_password").First(&row).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if row.Value == nil || *row.Value == plain {
		t.Error("迁移后库里仍是明文")
	}
	// 迁移同时清掉明文前值。
	if row.PreviousValue != nil {
		t.Errorf("迁移后仍保留了明文前值: %q", *row.PreviousValue)
	}
	// 值本身不能丢：邮件还得能发出去。
	if cfg, _ := svc.MailConfig(ctx); cfg.Password != plain {
		t.Errorf("迁移后解出的口令 = %q, 期望 %q", cfg.Password, plain)
	}

	// 幂等：再跑一次不该重复加密（重复加密会让值变成「密文的密文」，
	// 读出来是一串 base64，看起来像口令错了）。
	n2, err := svc.EncryptLegacySecrets(ctx)
	if err != nil {
		t.Fatalf("二次迁移失败: %v", err)
	}
	if n2 != 0 {
		t.Errorf("二次迁移条数 = %d, 期望 0（幂等）", n2)
	}
	if cfg, _ := svc.MailConfig(ctx); cfg.Password != plain {
		t.Errorf("二次迁移后口令被破坏: %q", cfg.Password)
	}
}

// TestNonSecretValueStaysPlaintext 覆盖「只加密敏感项」。
//
// 把所有设置都加密会带来两个实际问题：运维无法再用一条 SQL 核对配置，
// 而加密本身对这些值没有任何收益（它们本来就要展示给用户）。
func TestNonSecretValueStaysPlaintext(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{
		"basic.site_url": "https://panel.example.com",
	}, 1, "admin", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if got := secretAtRest(t, db, "basic.site_url"); got != "https://panel.example.com" {
		t.Errorf("非敏感项被加密了: %q", got)
	}
}

// TestCiphertextIsBoundToTheKey 证明加密不是摆设。
//
// 用另一把根密钥构造的实例必须**解不开**同一个库里的密文：那说明密钥真的
// 参与了运算，而不是把明文做了个 base64。
//
// 解不开时退回原文（plainValue 的兼容分支），因此这里断言的是「拿到的是
// 一串密文而不是原口令」——它会在 SMTP 认证上明确失败，而不是静默地用
// 一个错误的密码去试。
func TestCiphertextIsBoundToTheKey(t *testing.T) {
	svc, db := newTestEnvWithSecret(t, "key-A-0123456789")
	ctx := context.Background()

	const password = "Bound-To-Key-7777"
	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_password": password,
	}, 1, "admin", ""); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	raw := secretAtRest(t, db, "notification.smtp_password")
	// 同一把密钥能解开。
	keyA := cryptoutil.DeriveKey([]byte("key-A-0123456789"), "settings-secret")
	if plain, err := cryptoutil.Open(keyA, raw); err != nil || plain != password {
		t.Fatalf("同密钥解不开自己写的密文: %q / %v", plain, err)
	}
	// 换一把密钥解不开。
	keyB := cryptoutil.DeriveKey([]byte("key-B-9876543210"), "settings-secret")
	if _, err := cryptoutil.Open(keyB, raw); err == nil {
		t.Error("换密钥仍能解开 —— 密钥没有真正参与加密")
	}

	// 另一个实例读这个库：拿到的不是原口令。
	other := settings.NewService(db, nil, []byte("key-B-9876543210"))
	cfg, err := other.MailConfig(ctx)
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	if cfg.Password == password {
		t.Error("换密钥的实例读出了正确口令 —— 密文没有绑定密钥")
	}
}

// TestNewServiceRejectsEmptySecret 覆盖启动期快速失败。
//
// 空密钥会让敏感项退回明文，而且**没有任何一处会报错**。宁可在装配阶段
// 崩掉，也不要带着一个静默的降级跑起来。
func TestNewServiceRejectsEmptySecret(t *testing.T) {
	_, db := newTestEnv(t)

	defer func() {
		if p := recover(); p == nil {
			t.Error("空密钥应 panic，而不是构造出一个会存明文的实例")
		}
	}()
	settings.NewService(db, nil, nil)
}

// asAPIError 是 errors.As 的薄封装，避免本文件再导入 errors。
func asAPIError(err error, target **api.Error) bool {
	for err != nil {
		if e, ok := err.(*api.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
