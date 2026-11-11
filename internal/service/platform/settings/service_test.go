package settings_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/settings"
)

func newTestEnv(t *testing.T) (*settings.Service, *gorm.DB) {
	t.Helper()
	return newTestEnvWithSecret(t, "settings-test-secret")
}

// newTestEnvWithSecret 用指定根密钥构造服务，供加密相关的用例验证
// 「换了密钥就读不出旧值」这件事。
func newTestEnvWithSecret(t *testing.T, secret string) (*settings.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "settings.db"),
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
	if err := db.AutoMigrate(&model.SystemSetting{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	return settings.NewService(db, audit.NewRecorder(db), []byte(secret)), db
}

func itemOf(t *testing.T, items []settings.Item, key string) settings.Item {
	t.Helper()
	for _, it := range items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("清单中缺少 %s", key)
	return settings.Item{}
}

// --- 优先级（R-001）---

func TestDefaultValueWhenNeverSet(t *testing.T) {
	svc, _ := newTestEnv(t)

	items, _, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	it := itemOf(t, items, "basic.site_url")
	if it.Source != settings.SourceDefault {
		t.Errorf("来源 = %q, 期望 default", it.Source)
	}
	if it.Value != "" {
		t.Errorf("值 = %q", it.Value)
	}
}

func TestSettingOverridesDefault(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{"basic.site_url": "我的面板"}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	item, err := svc.Get(ctx, "basic.site_url")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.Source != settings.SourceSetting || item.Value != "我的面板" {
		t.Errorf("来源 = %q 值 = %q", item.Source, item.Value)
	}
}

func TestEnvVarWinsAndLocks(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	// 先写入一个面板设置。
	if _, err := svc.Update(ctx, map[string]string{"basic.site_url": "面板设置的值"}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	// 再设置环境变量：优先级更高（R-001），且**锁定**该项（R-002）。
	t.Setenv("SITE_URL", "环境变量的值")

	item, err := svc.Get(ctx, "basic.site_url")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.Source != settings.SourceEnv {
		t.Errorf("来源 = %q, 期望 env", item.Source)
	}
	if item.Value != "环境变量的值" {
		t.Errorf("值 = %q, 期望环境变量的值", item.Value)
	}
	if !item.Locked || item.LockedBy != "SITE_URL" {
		t.Errorf("锁定状态不正确: locked=%v by=%q", item.Locked, item.LockedBy)
	}
}

func TestLockedItemRejectsUpdate(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	t.Setenv("SITE_URL", "环境变量的值")

	results, err := svc.Update(ctx, map[string]string{"basic.site_url": "试图覆盖"}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("结果数 = %d", len(results))
	}

	// **拒绝**而不是「接受但不生效」（R-002）：后者比直接拒绝更糟——
	// 用户以为改成功了，直到某天发现系统行为与设置不符。
	if results[0].Status != settings.StatusFailed {
		t.Errorf("状态 = %q, 期望 failed", results[0].Status)
	}
	// 错误文案要告诉他去哪儿改。
	if results[0].Message == "" {
		t.Error("拒绝时未说明原因")
	}

	// 值必须没有改变。
	item, _ := svc.Get(ctx, "basic.site_url")
	if item.Value != "环境变量的值" {
		t.Errorf("被锁定的项竟然被改成了 %q", item.Value)
	}
}

// --- 部分成功（R-006）---

func TestUpdateIsPartiallySuccessful(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	t.Setenv("STORAGE_AUTO_TRIM_ENABLED", "true")

	results, err := svc.Update(ctx, map[string]string{
		"basic.site_url":             "改名成功",
		"storage.auto_trim_enabled":  "false", // 被环境变量锁定
		"vm.stale_threshold_seconds": "45",
	}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	byKey := map[string]settings.UpdateResult{}
	for _, r := range results {
		byKey[r.Key] = r
	}

	if byKey["basic.site_url"].Status != settings.StatusApplied {
		t.Errorf("basic.site_url 应成功: %+v", byKey["basic.site_url"])
	}
	if byKey["storage.auto_trim_enabled"].Status != settings.StatusFailed {
		t.Errorf("被锁定的项应失败: %+v", byKey["storage.auto_trim_enabled"])
	}
	// 一项失败**不得**影响其余项：原子语义会让用户不知道哪些生效了，
	// 只能逐项重试去试探。
	if byKey["vm.stale_threshold_seconds"].Status != settings.StatusApplied {
		t.Errorf("其余项应不受影响: %+v", byKey["vm.stale_threshold_seconds"])
	}
}

func TestUpdateValidatesTypes(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	cases := []struct {
		key   string
		value string
	}{
		{"vm.stale_threshold_seconds", "abc"},   // 非整数
		{"vm.stale_threshold_seconds", "0"},     // 低于下限
		{"vm.stale_threshold_seconds", "9999"},  // 高于上限
		{"storage.auto_trim_enabled", "是"},      // 非布尔
		{"notification.smtp_security", "adhoc"}, // 不在枚举内
	}

	for _, tc := range cases {
		results, err := svc.Update(ctx, map[string]string{tc.key: tc.value}, 1, "admin", "10.0.0.1")
		if err != nil {
			t.Fatalf("调用失败: %v", err)
		}
		if results[0].Status != settings.StatusFailed {
			t.Errorf("%s=%q 应被拒绝, 实际 %+v", tc.key, tc.value, results[0])
		}
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	svc, _ := newTestEnv(t)

	results, err := svc.Update(context.Background(),
		map[string]string{"not.a.real.key": "x"}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if results[0].Status != settings.StatusFailed {
		t.Errorf("未登记的键应被拒绝: %+v", results[0])
	}
}

// --- 回滚（R-008 / R-007）---

func TestRollbackRestoresPreviousValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	svc.Update(ctx, map[string]string{"basic.site_url": "第一次"}, 1, "admin", "10.0.0.1")
	svc.Update(ctx, map[string]string{"basic.site_url": "第二次"}, 1, "admin", "10.0.0.1")

	item, err := svc.Rollback(ctx, "basic.site_url", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if item.Value != "第一次" {
		t.Errorf("回滚后的值 = %q, 期望 第一次", item.Value)
	}

	// 回滚本身也是一次变更：回滚错了还能再回滚回来。不这么做的话，
	// 一次误回滚会让原值永久消失——而它本可以被简单恢复。
	item, err = svc.Rollback(ctx, "basic.site_url", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("二次回滚失败: %v", err)
	}
	if item.Value != "第二次" {
		t.Errorf("二次回滚后的值 = %q, 期望 第二次", item.Value)
	}
}

func TestRollbackWithoutHistoryFails(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.Rollback(context.Background(), "basic.site_url", 1, "admin", "10.0.0.1")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("无历史时应返回 422, 实际 %v", err)
	}
}

func TestIdenticalValueDoesNotMoveRollbackPoint(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	svc.Update(ctx, map[string]string{"basic.site_url": "A"}, 1, "admin", "10.0.0.1")
	svc.Update(ctx, map[string]string{"basic.site_url": "B"}, 1, "admin", "10.0.0.1")
	// 把同一个值再提交一次：前端整表回传时很常见。
	svc.Update(ctx, map[string]string{"basic.site_url": "B"}, 1, "admin", "10.0.0.1")

	item, err := svc.Rollback(ctx, "basic.site_url", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 若把「没有变化的重提交」也当作一次变更，回滚点会被推进到 B，
	// 于是「回滚」看起来什么都没做。
	if item.Value != "A" {
		t.Errorf("回滚后的值 = %q, 期望 A（回滚点不应被无变化的重提交推进）", item.Value)
	}
}

// applierStub 是一个可控的应用钩子。
type applierStub struct {
	fail bool
	last string
}

func (a *applierStub) Apply(_ context.Context, _ string, value string) error {
	a.last = value
	if a.fail {
		return errors.New("端口不可用")
	}
	return nil
}

func TestApplyFailureRollsBack(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	svc.Update(ctx, map[string]string{"basic.site_url": "原始值"}, 1, "admin", "10.0.0.1")

	stub := &applierStub{fail: true}
	svc.RegisterApplier("basic.site_url", stub)

	results, err := svc.Update(ctx, map[string]string{"basic.site_url": "改坏的值"}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	// 应用失败必须**自动回滚**（R-007）：不回滚的话，用户面对的是一个
	// 「设置成功但系统异常」的局面，而他没有明显的补救手段。
	if results[0].Status != settings.StatusRolledBack {
		t.Errorf("状态 = %q, 期望 rolled_back", results[0].Status)
	}

	item, _ := svc.Get(ctx, "basic.site_url")
	if item.Value != "原始值" {
		t.Errorf("应用失败后值 = %q, 期望回滚到 原始值", item.Value)
	}
}

func TestApplySuccessKeepsValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	stub := &applierStub{}
	svc.RegisterApplier("basic.site_url", stub)

	if _, err := svc.Update(ctx, map[string]string{"basic.site_url": "新值"}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	if stub.last != "新值" {
		t.Errorf("应用钩子收到的值 = %q", stub.last)
	}
	item, _ := svc.Get(ctx, "basic.site_url")
	if item.Value != "新值" {
		t.Errorf("值 = %q, 期望 新值", item.Value)
	}
}

// --- 清单与 Provider ---

func TestDeliveredGroupIsVisible(t *testing.T) {
	svc, _ := newTestEnv(t)

	items, groups, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	// 通知分组已随邮件能力交付：它必须出现在界面上，否则管理员无从配置
	// SMTP，找回密码与邮箱绑定会一起变成「点了没反应」。
	found := false
	for _, it := range items {
		if it.Group == "notification" {
			found = true
		}
	}
	if !found {
		t.Error("已交付的 notification 分组未出现在清单中")
	}
	foundGroup := false
	for _, g := range groups {
		if g.Key == "notification" {
			foundGroup = true
		}
	}
	if !foundGroup {
		t.Error("已交付的 notification 分组未出现在 groups 中")
	}

	results, _ := svc.Update(context.Background(),
		map[string]string{"notification.smtp_host": "smtp.example.com"}, 1, "admin", "10.0.0.1")
	if results[0].Status != settings.StatusApplied {
		t.Errorf("已交付的设置项应可更新: %+v", results[0])
	}
}

// TestSecretBlankKeepsValue 覆盖整张表单回传的场景。
//
// 界面拿不到敏感项的明文，回传时那一栏必然为空。若按普通字段处理，
// 「改一下端口」就会顺手清空 SMTP 密码。
func TestSecretBlankKeepsValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_password": "s3cret",
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("设置密码失败: %v", err)
	}

	// 模拟前端整表回传：密码一栏为空。
	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_host":     "smtp.example.com",
		"notification.smtp_password": "",
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("回传失败: %v", err)
	}

	cfg, err := svc.MailConfig(ctx)
	if err != nil {
		t.Fatalf("读取邮件配置失败: %v", err)
	}
	if cfg.Password != "s3cret" {
		t.Errorf("留空后密码 = %q, 期望保持 s3cret", cfg.Password)
	}
	if cfg.Host != "smtp.example.com" {
		t.Errorf("主机 = %q", cfg.Host)
	}
}

func TestMailConfigUsesEnvPriority(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_host": "panel.example.com",
		"notification.smtp_port": "2525",
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 环境变量优先（R-001）：面板里存的值仍在，但不生效。
	t.Setenv("SMTP_HOST", "env.example.com")

	cfg, err := svc.MailConfig(ctx)
	if err != nil {
		t.Fatalf("读取邮件配置失败: %v", err)
	}
	if cfg.Host != "env.example.com" {
		t.Errorf("主机 = %q, 期望环境变量的 env.example.com", cfg.Host)
	}
	if cfg.Port != 2525 {
		t.Errorf("端口 = %d, 期望面板设置的 2525", cfg.Port)
	}
}

func TestProviderReadsEffectiveValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	// 默认值。
	if got := svc.Int(settings.KeyVMStaleThreshold, 999); got != 120 {
		t.Errorf("默认阈值 = %d, 期望 120", got)
	}

	svc.Update(ctx, map[string]string{settings.KeyVMStaleThreshold: "180"}, 1, "admin", "10.0.0.1")
	if got := svc.Int(settings.KeyVMStaleThreshold, 999); got != 180 {
		t.Errorf("更新后的阈值 = %d, 期望 180", got)
	}

	// 环境变量优先级更高。
	t.Setenv("VM_STALE_THRESHOLD_SECONDS", "300")
	if got := svc.Int(settings.KeyVMStaleThreshold, 999); got != 300 {
		t.Errorf("环境变量生效后的阈值 = %d, 期望 300", got)
	}

	// 非法值回退到 fallback，而不是让业务拿到一个奇怪的数字。
	svc.Update(ctx, map[string]string{"vm.stale_threshold_seconds": "60"}, 1, "admin", "10.0.0.1")
	if got := svc.Int("not.a.real.key", 7); got != 7 {
		t.Errorf("未知键应返回 fallback, 实际 %d", got)
	}
}

// --- 首尾空白 ---

// lastAuditAfter 读回某个设置项最近一次审计里的 AfterState。
func lastAuditAfter(t *testing.T, db *gorm.DB, key string) map[string]any {
	t.Helper()

	var row model.AuditLog
	if err := db.Where("resource_type = ? AND resource_name = ?", "setting", key).
		Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("读取 %s 的审计记录失败: %v", key, err)
	}
	if row.AfterState == nil {
		t.Fatalf("%s 的审计记录没有 after_state", key)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(*row.AfterState), &after); err != nil {
		t.Fatalf("解析 after_state 失败: %v", err)
	}
	return after
}

// TestUpdateTrimsWhitespace 覆盖「首尾空格被原样入库」。
//
// 实测库里 `notification.smtp_from_name` 存成 `"K Cockpit     "`（5 个尾随
// 空格）：界面上用户看不出结尾有几个空格，下次保存又把它带上，而它还会
// 流进发件人名称这种**看得见**的地方。
//
// 只裁首尾、不动中间：`"K  Cockpit"` 里那两个空格是有意的排版。
func TestUpdateTrimsWhitespace(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	results, err := svc.Update(ctx, map[string]string{
		"notification.smtp_from_name": "  k-cockpit  ",
	}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if results[0].Status != settings.StatusApplied {
		t.Fatalf("状态 = %q（%s）", results[0].Status, results[0].Message)
	}

	item, err := svc.Get(ctx, "notification.smtp_from_name")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.Value != "k-cockpit" {
		t.Errorf("生效值 = %q, 期望 trim 后的 k-cockpit", item.Value)
	}

	// 审计里记的必须是**实际生效的那个值**：审计说带空格、实际不带，
	// 就等于事后无法从审计还原系统当时的行为。
	after := lastAuditAfter(t, db, "notification.smtp_from_name")
	if got := after["value"]; got != "k-cockpit" {
		t.Errorf("审计中的新值 = %v, 期望 trim 后的 k-cockpit", got)
	}

	// 中间的空格不受影响。
	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_from_name": " K  Cockpit ",
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	item, _ = svc.Get(ctx, "notification.smtp_from_name")
	if item.Value != "K  Cockpit" {
		t.Errorf("生效值 = %q, 期望只裁首尾的 K  Cockpit", item.Value)
	}
}

// TestUpdateBlankSecretKeepsPrevious 覆盖「只有空格的密码」。
//
// trim 之前，"   " 不等于空串，于是它会被当成「用户要把密码改成三个空格」，
// 既改坏了配置，又让界面在下次回显时显示「已设置」。用户眼里那一栏就是
// 没填——那就该走「留空保持原值」同一条分支。
func TestUpdateBlankSecretKeepsPrevious(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{
		"notification.smtp_password": "s3cret",
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("设置密码失败: %v", err)
	}

	results, err := svc.Update(ctx, map[string]string{
		"notification.smtp_password": "   ",
	}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if results[0].Status != settings.StatusApplied {
		t.Fatalf("状态 = %q（%s）", results[0].Status, results[0].Message)
	}
	if !strings.Contains(results[0].Message, "保持原有值") {
		t.Errorf("文案 = %q, 应说明「留空，保持原有值不变」", results[0].Message)
	}

	// 库里的值必须**一个字都没动**。
	//
	// 注意这里不能直接比对明文：库里存的是密文（U-33）。要验的语义是
	// 「密文没被换成新值」——用 service 解回来还是 s3cret 即可，同时断言
	// 库里确实不是明文，防止加密被悄悄关掉。
	var row model.SystemSetting
	if err := db.Where("key = ?", "notification.smtp_password").First(&row).Error; err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if row.Value == nil {
		t.Fatal("库里没有密码")
	}
	if *row.Value == "s3cret" {
		t.Errorf("库里是明文密码 —— 敏感项必须加密存储")
	}

	// 真正生效的配置也没变。
	cfg, err := svc.MailConfig(ctx)
	if err != nil {
		t.Fatalf("读取邮件配置失败: %v", err)
	}
	if cfg.Password != "s3cret" {
		t.Errorf("生效密码 = %q, 期望 s3cret", cfg.Password)
	}
}
