package risk

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

// devBypassTestEnv 建一个只含 user 表的库，返回 Guard 与用户。
func devBypassTestEnv(t *testing.T, devCode string) (*Guard, context.Context) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "risk.db"),
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
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.User{
		Username: "alice", Role: model.RoleTenant,
	}).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	// recorder 传 nil：本测试只关心验证结果，不关心审计是否写入。
	guard := NewGuard(db, []byte("test-secret-for-devbypass"), nil, devCode)
	return guard, context.Background()
}

// loadUser 重新读取用户：变更都存在库里而不是内存对象上。
func loadUser(t *testing.T, g *Guard) *model.User {
	t.Helper()
	var u model.User
	if err := g.db.First(&u, "username = ?", "alice").Error; err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}
	return &u
}

// TestConfirmTOTPSetupAcceptsDevBypass 覆盖用户实际会卡住的那一步。
//
// 加它是因为：绑定确认是整个二次验证的**前置条件**。若万能码只在「验证」
// 处生效，用户会在绑定这一步被挡住，而前面又没有任何出路——所有高风险
// 操作都做不了，界面上却显示「未绑定」。
func TestConfirmTOTPSetupAcceptsDevBypass(t *testing.T) {
	g, ctx := devBypassTestEnv(t, "123456")

	user := loadUser(t, g)
	if _, err := g.BeginTOTPSetup(ctx, user); err != nil {
		t.Fatalf("开始绑定失败: %v", err)
	}

	codes, err := g.ConfirmTOTPSetup(ctx, loadUser(t, g), "123456")
	if err != nil {
		t.Fatalf("万能码未被绑定确认接受: %v", err)
	}
	// 恢复码照常生成：万能码跳过的是「能否算出动态码」的确认，
	// 不是把整个绑定流程变成空操作。
	if len(codes) == 0 {
		t.Error("绑定成功但未生成恢复码")
	}

	after := loadUser(t, g)
	if !after.TotpEnabled {
		t.Error("绑定确认通过后 totp_enabled 仍为 false")
	}
	if after.RecoveryCodesHash == nil || *after.RecoveryCodesHash == "" {
		t.Error("恢复码哈希未写入")
	}
}

// TestConfirmTOTPSetupStillRejectsWrongCode 确认万能码没有把绑定确认
// 变成「随便填都能过」——它只接受那一个值。
func TestConfirmTOTPSetupStillRejectsWrongCode(t *testing.T) {
	g, ctx := devBypassTestEnv(t, "123456")

	user := loadUser(t, g)
	if _, err := g.BeginTOTPSetup(ctx, user); err != nil {
		t.Fatalf("开始绑定失败: %v", err)
	}

	for _, code := range []string{"000000", "111111", "12345", "abc123"} {
		if _, err := g.ConfirmTOTPSetup(ctx, loadUser(t, g), code); err == nil {
			t.Errorf("码 %q 被错误地接受了", code)
		}
	}

	if loadUser(t, g).TotpEnabled {
		t.Error("失败的确认不应启用 TOTP")
	}
}

// TestConfirmTOTPSetupWithoutDevBypass 覆盖**生产环境的行为**：
// 开发码未配置时，那个值就只是一个普通错误码。
func TestConfirmTOTPSetupWithoutDevBypass(t *testing.T) {
	// 空配置 = 生产环境恒定的状态（config.Validate 保证）。
	g, ctx := devBypassTestEnv(t, "")

	user := loadUser(t, g)
	if _, err := g.BeginTOTPSetup(ctx, user); err != nil {
		t.Fatalf("开始绑定失败: %v", err)
	}

	_, err := g.ConfirmTOTPSetup(ctx, loadUser(t, g), "123456")
	if err == nil {
		t.Fatal("未配置万能码时 123456 不应被接受")
	}

	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T, 期望 *api.Error", err)
	}
	// 文案应当是「验证码不正确」，而不是任何暗示「有这个功能但没开」
	// 的说法——后者会让排查者以为配置漏了。
	if !strings.Contains(apiErr.Message, "验证码不正确") {
		t.Errorf("错误文案 = %q, 期望提示验证码不正确", apiErr.Message)
	}
}

// TestRecoveryCodesCoexistWithDevBypass 确认万能码与真实凭据并存：
// 开了万能码之后，已经生成的恢复码仍然可用。
//
// 若万能码的实现是「替换掉真实验证」，这里会失败——而那意味着开发环境
// 里所有真实凭据都被静默作废，测试覆盖不到真实路径。
func TestRecoveryCodesCoexistWithDevBypass(t *testing.T) {
	g, ctx := devBypassTestEnv(t, "123456")

	user := loadUser(t, g)
	if _, err := g.BeginTOTPSetup(ctx, user); err != nil {
		t.Fatalf("开始绑定失败: %v", err)
	}
	codes, err := g.ConfirmTOTPSetup(ctx, loadUser(t, g), "123456")
	if err != nil {
		t.Fatalf("绑定失败: %v", err)
	}

	// 恢复码是真实凭据，必须照常通过——这证明万能码是**追加**的入口，
	// 而不是把整个校验短路成「永远 OK」。
	if r := Verify(loadUser(t, g), "", MethodRecoveryCode, codes[0], time.Now()); !r.OK {
		t.Error("开发模式下恢复码失效了：万能码不应影响真实凭据")
	}
}
