package accesscontrol_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/accesscontrol"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

func newEnv(t *testing.T, opts accesscontrol.Options) (*gorm.DB, *accesscontrol.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "ac.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.SystemSetting{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db, accesscontrol.NewService(db, audit.NewRecorder(db), opts)
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

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

// TestIsPublicIP 覆盖地址判定。
//
// **判错的方向是相反的**：把代理地址当公网，用户在其实安全的连接上被拦；
// 把公网当内网，他在真的会被切断时毫无准备。
func TestIsPublicIP(t *testing.T) {
	private := []string{
		"127.0.0.1", "::1", "10.0.0.5", "192.168.1.1", "172.16.0.1",
		"169.254.1.1", "0.0.0.0",
		"192.168.1.1:8080", "10.0.0.5:1234",
		// 解析不出来的**按内网算**：被切断的代价（会话中断、可能再也连不上）
		// 明显大于多问一句的代价。
		"", "not-an-ip", "localhost",
	}
	for _, ip := range private {
		if accesscontrol.IsPublicIP(ip) {
			t.Errorf("%q 应判为内网/不可判定", ip)
		}
	}

	public := []string{"203.0.113.9", "8.8.8.8", "2001:4860:4860::8888", "203.0.113.9:51234"}
	for _, ip := range public {
		if !accesscontrol.IsPublicIP(ip) {
			t.Errorf("%q 应判为公网", ip)
		}
	}
}

// TestCuttingOwnConnectionNeedsConfirm 覆盖最重要的一条。
//
// 用户正是从公网访问时关掉公网访问，那一刻他的连接就断了——而界面还没
// 来得及显示"已保存"。不确认就**不执行也不报错**，而是返回现状：那是一个
// 岔路口，不是一次失败。
func TestCuttingOwnConnectionNeedsConfirm(t *testing.T) {
	db, svc := newEnv(t, accesscontrol.Options{})
	ctx := context.Background()

	// 先开着公网访问。
	on := true
	if _, err := svc.Set(ctx, accesscontrol.Request{PublicEnabled: &on}, true,
		"10.0.0.5", admin(), "root", "10.0.0.5"); err != nil {
		t.Fatalf("开启失败: %v", err)
	}

	// 内网调用方关闭：不需要确认。
	off := false
	if _, err := svc.Set(ctx, accesscontrol.Request{PublicEnabled: &off}, false,
		"10.0.0.5", admin(), "root", "10.0.0.5"); err != nil {
		t.Fatalf("内网关闭不该需要确认: %v", err)
	}
	var row model.SystemSetting
	db.Where("key = ?", accesscontrol.KeyPublicEnabled).First(&row)
	if row.Value == nil || *row.Value != "false" {
		t.Error("内网关闭应当生效")
	}

	// 重新开启，然后从**公网**关。
	if _, err := svc.Set(ctx, accesscontrol.Request{PublicEnabled: &on}, true,
		"203.0.113.9", admin(), "root", "203.0.113.9"); err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	view, err := svc.Set(ctx, accesscontrol.Request{PublicEnabled: &off}, false,
		"203.0.113.9", admin(), "root", "203.0.113.9")
	if err != nil {
		t.Fatalf("未确认时应返回现状而不是报错: %v", err)
	}
	if view == nil || !view.PublicEnabled {
		t.Fatal("未确认时不该执行")
	}
	if !view.CallerIsPublic {
		t.Error("应当告知调用方自己就在公网上——那是它需要确认的原因")
	}

	// 确认之后才执行。
	if _, err := svc.Set(ctx, accesscontrol.Request{PublicEnabled: &off}, true,
		"203.0.113.9", admin(), "root", "203.0.113.9"); err != nil {
		t.Fatalf("确认后应能关闭: %v", err)
	}
	db.Where("key = ?", accesscontrol.KeyPublicEnabled).First(&row)
	if row.Value == nil || *row.Value != "false" {
		t.Error("确认后应当生效")
	}
}

// TestEnvLockWins 覆盖环境变量优先。
//
// 部署方在启动参数里关掉公网访问之后，面板上那个开关不该能把它打开——
// 否则"已在启动参数里关闭"只是一句建议。
func TestEnvLockWins(t *testing.T) {
	yes := true
	no := false
	db, svc := newEnv(t, accesscontrol.Options{EnvPublicEnabled: &no, EnvDevMode: &yes})
	ctx := context.Background()

	view, err := svc.Get(ctx, "10.0.0.5")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !view.EnvLocked {
		t.Error("应标出被环境变量锁定——否则界面上的开关会让人以为能改")
	}
	if view.EffectedBy != "环境变量" {
		t.Errorf("来源应为环境变量，实际 %q", view.EffectedBy)
	}
	// 环境变量的值就是权威。
	if view.PublicEnabled {
		t.Error("应以环境变量的 false 为准")
	}
	if !view.DevMode {
		t.Error("应以环境变量的 true 为准")
	}

	on := true
	_, err = svc.Set(ctx, accesscontrol.Request{PublicEnabled: &on}, true,
		"10.0.0.5", admin(), "root", "10.0.0.5")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "环境变量") {
		t.Errorf("报错应说明是环境变量优先: %v", err)
	}
	// 库里不该被写入。
	var n int64
	db.Model(&model.SystemSetting{}).Where("key = ?", accesscontrol.KeyPublicEnabled).Count(&n)
	if n != 0 {
		t.Error("被锁定时不该写库——那会让环境变量与库出现两份不一致的值")
	}
}

// TestAuditRecordsCutting 覆盖审计里记「这次改会不会切断调用方」。
//
// 事后看审计时，一条"改了公网开关"和一个"人突然掉线了"能否对上，靠的就是它。
func TestAuditRecordsCutting(t *testing.T) {
	db, svc := newEnv(t, accesscontrol.Options{})
	ctx := context.Background()

	on := true
	svc.Set(ctx, accesscontrol.Request{PublicEnabled: &on}, true, "10.0.0.5", admin(), "root", "10.0.0.5")
	off := false
	if _, err := svc.Set(ctx, accesscontrol.Request{PublicEnabled: &off}, true,
		"203.0.113.9", admin(), "root", "203.0.113.9"); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	var rec model.AuditLog
	if err := db.Where("action = ?", "access_control.update").
		Order("id DESC").First(&rec).Error; err != nil {
		t.Fatalf("应记审计: %v", err)
	}
	if rec.Params == nil || !strings.Contains(*rec.Params, "cut_own_connection") {
		t.Errorf("审计应记下是否切断了调用方: %v", rec.Params)
	}
	if !strings.Contains(*rec.Params, "true") {
		t.Errorf("这次确实切断了（公网关公网），应记为 true: %v", rec.Params)
	}
}

func TestNoChangesRejected(t *testing.T) {
	_, svc := newEnv(t, accesscontrol.Options{})

	_, err := svc.Set(context.Background(), accesscontrol.Request{}, true,
		"10.0.0.5", admin(), "root", "10.0.0.5")
	assertStatus(t, err, 400)
}

func TestDevModeToggle(t *testing.T) {
	db, svc := newEnv(t, accesscontrol.Options{})
	ctx := context.Background()

	on := true
	view, err := svc.Set(ctx, accesscontrol.Request{DevMode: &on}, false,
		"10.0.0.5", admin(), "root", "10.0.0.5")
	if err != nil {
		t.Fatalf("开启开发模式失败: %v", err)
	}
	if !view.DevMode {
		t.Error("应已开启")
	}
	// 开发模式**不需要确认**：开它不会切断任何人。
	var row model.SystemSetting
	db.Where("key = ?", accesscontrol.KeyDevMode).First(&row)
	if row.Value == nil || *row.Value != "true" {
		t.Error("应写入库")
	}
}
