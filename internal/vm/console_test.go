package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// consoleFixture 准备一台可用于控制台测试的虚拟机。
func consoleFixture(t *testing.T, display string, enabled bool) (*vm.Service, *gorm.DB, *model.VM) {
	t.Helper()

	svc, _, db := newTestEnv(t)
	svc.SetEncryptionKey(cryptoutil.DeriveKey([]byte("test-key-for-console"), "v1"))

	row := &model.VM{
		NodeID:        1,
		Name:          "vm-console",
		Status:        model.VMStatusRunning,
		OwnerID:       ptr(int64(7)),
		DisplayDevice: display,
		VNCEnabled:    enabled,
		VNCBind:       "127.0.0.1",
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("准备虚拟机失败: %v", err)
	}
	return svc, db, row
}

// --- 显示设备为 none（R-011）---

func TestConsoleUnavailableWithoutDisplayDevice(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayNone, false)

	cfg, err := svc.Console(context.Background(), row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	// 没有图形显示设备的虚拟机**没有控制台**。这里必须给出原因，而不是
	// 返回一份「可用但打不开」的配置——那会让界面显示一个点了没反应的按钮。
	if cfg.Available {
		t.Error("无显示设备的虚拟机不应标记为可用")
	}
	if cfg.UnavailableReason == "" {
		t.Error("未说明不可用原因")
	}
}

func TestOpenConsoleRejectedWithoutDisplay(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayNone, true)

	_, _, err := svc.OpenConsole(context.Background(), row.ID, 7, authz.Viewer{UserID: 7})
	assertAPIError(t, err, 422)
}

// --- 开启状态与传输能力 ---

func TestOpenConsoleRejectedWhenDisabled(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, false)

	_, _, err := svc.OpenConsole(context.Background(), row.ID, 7, authz.Viewer{UserID: 7})
	assertAPIError(t, err, 422)
}

func TestOpenConsoleReportsUnsupportedTransport(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, true)

	// mock agent 不支持流式转发。此时**必须区分**「传输不支持」与
	// 「控制台没开」：让用户去检查「控制台开没开」是误导，它明明已经开了。
	_, _, err := svc.OpenConsole(context.Background(), row.ID, 7, authz.Viewer{UserID: 7})
	assertAPIError(t, err, 503)

	var apiErr *api.Error
	if errors.As(err, &apiErr) &&
		!strings.Contains(apiErr.Message, "模拟") && !strings.Contains(apiErr.Message, "不支持") {
		t.Errorf("错误文案未说明是传输能力问题: %q", apiErr.Message)
	}
}

func TestConsoleConfigCarriesSessionLimit(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, true)

	cfg, err := svc.Console(context.Background(), row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if cfg.SessionLimit != vm.ConsoleSessionLimit {
		t.Errorf("会话上限 = %d, 期望 %d", cfg.SessionLimit, vm.ConsoleSessionLimit)
	}
	if cfg.ActiveSessions != 0 {
		t.Errorf("初始会话数 = %d, 期望 0", cfg.ActiveSessions)
	}
}

// --- 密码只写不读（R-005）---

func TestConsolePasswordIsWriteOnly(t *testing.T) {
	svc, db, row := consoleFixture(t, model.DisplayVNC, true)
	ctx := context.Background()

	password := "s3cret"
	cfg, err := svc.UpdateConsole(ctx, row.ID, vm.ConsoleUpdate{Password: &password},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("设置密码失败: %v", err)
	}

	// 能看出「已设置」，但拿不到明文——密码一旦可读，加密存储的意义就
	// 只剩下防止直接拖库，而最大的泄漏面恰恰是通过接口。
	if !cfg.HasPassword {
		t.Error("设置后应报告已设置密码")
	}

	// 库里存的必须是密文。
	var cred model.VMCredential
	if err := db.Where("vm_id = ?", row.ID).First(&cred).Error; err != nil {
		t.Fatalf("凭据记录未写入: %v", err)
	}
	if strings.Contains(cred.PasswordEnc, password) {
		t.Error("密码以明文形式落库")
	}
}

func TestConsolePasswordTooLongRejected(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, true)

	// VNC 认证在多数实现上只使用前 8 个字符。接受更长的密码会给用户
	// 「我设了长密码」的错觉，而实际生效的只是前 8 位。
	long := "123456789"
	_, err := svc.UpdateConsole(context.Background(), row.ID, vm.ConsoleUpdate{Password: &long},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1")
	assertAPIError(t, err, 400)
}

func TestConsolePasswordWithoutKeyFails(t *testing.T) {
	// 未配置加密密钥时不能静默保存明文：那会让「加密存储」变成一句
	// 只在配置正确时才成立的话。
	svc, _, db := newTestEnv(t)

	row := model.VM{
		NodeID: 1, Name: "vm-nokey", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), DisplayDevice: model.DisplayVNC, VNCEnabled: true,
		VNCBind: "127.0.0.1",
	}
	db.Create(&row)

	password := "abc"
	_, err := svc.UpdateConsole(context.Background(), row.ID, vm.ConsoleUpdate{Password: &password},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1")
	assertAPIError(t, err, 500)

	var count int64
	db.Model(&model.VMCredential{}).Count(&count)
	if count != 0 {
		t.Error("密钥缺失时仍然写入了凭据")
	}
}

// --- 对外暴露（R-004）---

func TestExposureRequiresVerification(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, true)
	ctx := context.Background()

	exposed := true

	// 未标记「已验证」：拒绝。检查在**服务层**而不是只在 handler——
	// 把安全判定放在一个入口上，等于给另一个入口留了缺口。
	_, err := svc.UpdateConsole(ctx, row.ID, vm.ConsoleUpdate{Exposed: &exposed},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1")
	assertAPIError(t, err, 422)

	// 标记后放行，且监听地址随之改变。
	verified := vm.WithExposureVerified(ctx)
	cfg, err := svc.UpdateConsole(verified, row.ID, vm.ConsoleUpdate{Exposed: &exposed},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("验证后应放行: %v", err)
	}
	if !cfg.Exposed {
		t.Error("暴露状态未更新")
	}
	if cfg.Bind != "0.0.0.0" {
		t.Errorf("监听地址 = %q, 期望 0.0.0.0", cfg.Bind)
	}
}

func TestDisableExposureRestoresLoopback(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, true)
	ctx := vm.WithExposureVerified(context.Background())

	// 先暴露。
	on := true
	if _, err := svc.UpdateConsole(ctx, row.ID, vm.ConsoleUpdate{Exposed: &on},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("开启暴露失败: %v", err)
	}

	off := false
	cfg, err := svc.UpdateConsole(ctx, row.ID, vm.ConsoleUpdate{Exposed: &off},
		authz.Viewer{UserID: 7}, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("关闭暴露失败: %v", err)
	}

	// 关闭暴露必须**收回监听地址**，否则「已关闭」只是一个字段值，
	// 端口仍然开着——而那正是最危险的一种「看起来安全」。
	if cfg.Exposed {
		t.Error("暴露状态未关闭")
	}
	if cfg.Bind != "127.0.0.1" {
		t.Errorf("监听地址 = %q, 期望收回 127.0.0.1", cfg.Bind)
	}
}

// --- 权限（R-003）---

func TestConsoleRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)

	row := model.VM{
		NodeID: 1, Name: "vm-theirs", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(20)), DisplayDevice: model.DisplayVNC, VNCEnabled: true,
		VNCBind: "127.0.0.1",
	}
	db.Create(&row)

	ctx := context.Background()

	// 越权一律 404（与详情页一致）：403 会告诉对方「这台虚拟机确实存在」，
	// 从而可以被用来枚举他人资源。
	_, err := svc.Console(ctx, row.ID, authz.Viewer{UserID: 10})
	assertAPIError(t, err, 404)

	_, _, err = svc.OpenConsole(ctx, row.ID, 10, authz.Viewer{UserID: 10})
	assertAPIError(t, err, 404)
}

// --- 会话名额的释放（R-014）---

func TestFailedConnectionReleasesSessionSlot(t *testing.T) {
	svc, _, row := consoleFixture(t, model.DisplayVNC, true)
	ctx := context.Background()

	// 每次打开都因传输不支持而失败，但**失败必须释放会话名额**——否则
	// 一次连不上的尝试会永久占掉一个名额，用户反复重试直到撞上上限，
	// 而界面上看不到任何正在使用的会话。
	for i := 0; i < vm.ConsoleSessionLimit+2; i++ {
		_, _, err := svc.OpenConsole(ctx, row.ID, 7, authz.Viewer{UserID: 7})
		assertAPIError(t, err, 503)
	}

	cfg, err := svc.Console(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if cfg.ActiveSessions != 0 {
		t.Errorf("失败的连接占用了 %d 个会话名额", cfg.ActiveSessions)
	}
}
