package settings_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/settings"
)

func newTestEnv(t *testing.T) (*settings.Service, *gorm.DB) {
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
	if err := db.AutoMigrate(&model.SystemSetting{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	return settings.NewService(db, audit.NewRecorder(db)), db
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

	it := itemOf(t, items, "site.name")
	if it.Source != settings.SourceDefault {
		t.Errorf("来源 = %q, 期望 default", it.Source)
	}
	if it.Value != "K Cockpit" {
		t.Errorf("值 = %q", it.Value)
	}
}

func TestSettingOverridesDefault(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Update(ctx, map[string]string{"site.name": "我的面板"}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	item, err := svc.Get(ctx, "site.name")
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
	if _, err := svc.Update(ctx, map[string]string{"site.name": "面板设置的值"}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	// 再设置环境变量：优先级更高（R-001），且**锁定**该项（R-002）。
	t.Setenv("SITE_NAME", "环境变量的值")

	item, err := svc.Get(ctx, "site.name")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.Source != settings.SourceEnv {
		t.Errorf("来源 = %q, 期望 env", item.Source)
	}
	if item.Value != "环境变量的值" {
		t.Errorf("值 = %q, 期望环境变量的值", item.Value)
	}
	if !item.Locked || item.LockedBy != "SITE_NAME" {
		t.Errorf("锁定状态不正确: locked=%v by=%q", item.Locked, item.LockedBy)
	}
}

func TestLockedItemRejectsUpdate(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	t.Setenv("SITE_NAME", "环境变量的值")

	results, err := svc.Update(ctx, map[string]string{"site.name": "试图覆盖"}, 1, "admin", "10.0.0.1")
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
	item, _ := svc.Get(ctx, "site.name")
	if item.Value != "环境变量的值" {
		t.Errorf("被锁定的项竟然被改成了 %q", item.Value)
	}
}

// --- 部分成功（R-006）---

func TestUpdateIsPartiallySuccessful(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	t.Setenv("RISK_VERIFICATION_ENABLED", "true")

	results, err := svc.Update(ctx, map[string]string{
		"site.name":                      "改名成功",
		"auth.risk_verification_enabled": "false", // 被环境变量锁定
		"session.idle_timeout_minutes":   "45",
	}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	byKey := map[string]settings.UpdateResult{}
	for _, r := range results {
		byKey[r.Key] = r
	}

	if byKey["site.name"].Status != settings.StatusApplied {
		t.Errorf("site.name 应成功: %+v", byKey["site.name"])
	}
	if byKey["auth.risk_verification_enabled"].Status != settings.StatusFailed {
		t.Errorf("被锁定的项应失败: %+v", byKey["auth.risk_verification_enabled"])
	}
	// 一项失败**不得**影响其余项：原子语义会让用户不知道哪些生效了，
	// 只能逐项重试去试探。
	if byKey["session.idle_timeout_minutes"].Status != settings.StatusApplied {
		t.Errorf("其余项应不受影响: %+v", byKey["session.idle_timeout_minutes"])
	}
}

func TestUpdateValidatesTypes(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	cases := []struct {
		key   string
		value string
	}{
		{"session.idle_timeout_minutes", "abc"},  // 非整数
		{"session.idle_timeout_minutes", "0"},    // 低于下限
		{"session.idle_timeout_minutes", "9999"}, // 高于上限
		{"auth.risk_verification_enabled", "是"},  // 非布尔
		{"network.default_mode", "adhoc"},        // 不在枚举内
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

	svc.Update(ctx, map[string]string{"site.name": "第一次"}, 1, "admin", "10.0.0.1")
	svc.Update(ctx, map[string]string{"site.name": "第二次"}, 1, "admin", "10.0.0.1")

	item, err := svc.Rollback(ctx, "site.name", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if item.Value != "第一次" {
		t.Errorf("回滚后的值 = %q, 期望 第一次", item.Value)
	}

	// 回滚本身也是一次变更：回滚错了还能再回滚回来。不这么做的话，
	// 一次误回滚会让原值永久消失——而它本可以被简单恢复。
	item, err = svc.Rollback(ctx, "site.name", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("二次回滚失败: %v", err)
	}
	if item.Value != "第二次" {
		t.Errorf("二次回滚后的值 = %q, 期望 第二次", item.Value)
	}
}

func TestRollbackWithoutHistoryFails(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.Rollback(context.Background(), "site.name", 1, "admin", "10.0.0.1")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("无历史时应返回 422, 实际 %v", err)
	}
}

func TestIdenticalValueDoesNotMoveRollbackPoint(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	svc.Update(ctx, map[string]string{"site.name": "A"}, 1, "admin", "10.0.0.1")
	svc.Update(ctx, map[string]string{"site.name": "B"}, 1, "admin", "10.0.0.1")
	// 把同一个值再提交一次：前端整表回传时很常见。
	svc.Update(ctx, map[string]string{"site.name": "B"}, 1, "admin", "10.0.0.1")

	item, err := svc.Rollback(ctx, "site.name", 1, "admin", "10.0.0.1")
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

	svc.Update(ctx, map[string]string{"site.name": "原始值"}, 1, "admin", "10.0.0.1")

	stub := &applierStub{fail: true}
	svc.RegisterApplier("site.name", stub)

	results, err := svc.Update(ctx, map[string]string{"site.name": "改坏的值"}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	// 应用失败必须**自动回滚**（R-007）：不回滚的话，用户面对的是一个
	// 「设置成功但系统异常」的局面，而他没有明显的补救手段。
	if results[0].Status != settings.StatusRolledBack {
		t.Errorf("状态 = %q, 期望 rolled_back", results[0].Status)
	}

	item, _ := svc.Get(ctx, "site.name")
	if item.Value != "原始值" {
		t.Errorf("应用失败后值 = %q, 期望回滚到 原始值", item.Value)
	}
}

func TestApplySuccessKeepsValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	stub := &applierStub{}
	svc.RegisterApplier("site.name", stub)

	if _, err := svc.Update(ctx, map[string]string{"site.name": "新值"}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}

	if stub.last != "新值" {
		t.Errorf("应用钩子收到的值 = %q", stub.last)
	}
	item, _ := svc.Get(ctx, "site.name")
	if item.Value != "新值" {
		t.Errorf("值 = %q, 期望 新值", item.Value)
	}
}

// --- 清单与 Provider ---

func TestUndeliveredGroupIsHidden(t *testing.T) {
	svc, _ := newTestEnv(t)

	items, groups, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	// 未交付标签的设置项**不出现在界面**（R-005），而不是显示为不可用：
	// 一个灰掉的开关只会让用户以为功能坏了。
	for _, it := range items {
		if it.Group == "notification" {
			t.Errorf("未交付标签的设置项出现在清单中: %s", it.Key)
		}
	}
	for _, g := range groups {
		if g.Key == "notification" {
			t.Error("未交付的分组出现在 groupes 中")
		}
	}

	// 未交付的项也不能被更新：它不在清单里，服务端也不该接受。
	results, _ := svc.Update(context.Background(),
		map[string]string{"notification.smtp_host": "smtp.example.com"}, 1, "admin", "10.0.0.1")
	if results[0].Status != settings.StatusFailed {
		t.Errorf("未交付的设置项应不可更新: %+v", results[0])
	}
}

func TestProviderReadsEffectiveValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	// 默认值。
	if got := svc.Int(settings.KeyVMStaleThreshold, 999); got != 60 {
		t.Errorf("默认阈值 = %d, 期望 60", got)
	}

	svc.Update(ctx, map[string]string{settings.KeyVMStaleThreshold: "120"}, 1, "admin", "10.0.0.1")
	if got := svc.Int(settings.KeyVMStaleThreshold, 999); got != 120 {
		t.Errorf("更新后的阈值 = %d, 期望 120", got)
	}

	// 环境变量优先级更高。
	t.Setenv("VM_STALE_THRESHOLD_SECONDS", "300")
	if got := svc.Int(settings.KeyVMStaleThreshold, 999); got != 300 {
		t.Errorf("环境变量生效后的阈值 = %d, 期望 300", got)
	}

	// 非法值回退到 fallback，而不是让业务拿到一个奇怪的数字。
	svc.Update(ctx, map[string]string{"auth.max_login_attempts": "5"}, 1, "admin", "10.0.0.1")
	if got := svc.Int("not.a.real.key", 7); got != 7 {
		t.Errorf("未知键应返回 fallback, 实际 %d", got)
	}
}
