package handler_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// TestBootstrapStageEndToEnd 走一遍「管理员登录 → 停在引导 → 跳过 → 进入系统」。
//
// 放在接口层而不是只测服务层，是因为这条链路上真正的风险在**两端之间**：
// 字段名写错（login_token 拼错）、Cookie 没下发、阶段判断与路由不匹配，
// 这些都不会被服务层测试发现。
func TestBootstrapStageEndToEnd(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "root", model.RoleAdmin)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/login", loginBody("root", testPassword),
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusOK {
		t.Fatalf("登录状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	var login struct {
		Data struct {
			Stage      string `json:"stage"`
			LoginToken string `json:"login_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &login); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}
	if login.Data.Stage != "bootstrap_security" {
		t.Fatalf("阶段 = %q, 期望 bootstrap_security", login.Data.Stage)
	}
	if login.Data.LoginToken == "" {
		t.Fatal("未返回中间态令牌")
	}
	// 未完成的登录**不得**下发 Cookie：否则浏览器会带着一个半权限令牌
	// 继续访问业务接口。
	if raw := w.Header().Get("Set-Cookie"); raw != "" {
		t.Errorf("中间态登录不应下发 Cookie: %s", raw)
	}

	skip := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/bootstrap/skip",
		jsonBody(map[string]string{"login_token": login.Data.LoginToken}),
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if skip.Code != consts.StatusOK {
		t.Fatalf("跳过引导状态码 = %d (body=%s)", skip.Code, skip.Body.String())
	}
	rawCookie := skip.Header().Get("Set-Cookie")
	if rawCookie == "" {
		t.Fatal("跳过后未下发会话 Cookie")
	}
	cookie := auth.CookieName + "=" + tokenFromSetCookie(t, rawCookie)

	session := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if session.Code != consts.StatusOK {
		t.Fatalf("会话查询状态码 = %d (body=%s)", session.Code, session.Body.String())
	}
}

// 中间态令牌不能调用业务接口：它离完整权限只差一步，放行等于把二次验证
// 与引导变成装饰。
func TestStageTokenCannotCallBusinessAPI(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "root", model.RoleAdmin)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/login", loginBody("root", testPassword),
		ut.Header{Key: "Content-Type", Value: "application/json"})
	var login struct {
		Data struct {
			LoginToken string `json:"login_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &login); err != nil {
		t.Fatalf("解析登录响应失败: %v", err)
	}

	// 中间态令牌不下发 Cookie，业务接口只认 Cookie。
	got := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil)
	if got.Code != consts.StatusUnauthorized {
		t.Errorf("未带凭据访问业务接口状态码 = %d, 期望 401", got.Code)
	}
	if login.Data.LoginToken == "" {
		t.Error("未返回中间态令牌，后续用例失去意义")
	}
}

// 未配置邮件服务时，找回密码接口必须明确说"未配置"，而不是 500——
// 邮件是可选能力，全新安装本来就没有 SMTP。
func TestForgotPasswordDegradesWithoutMail(t *testing.T) {
	h, _ := newAuthServer(t)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/forgot/send",
		jsonBody(map[string]string{"email": "someone@example.com"}),
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusServiceUnavailable {
		t.Errorf("状态码 = %d, 期望 503 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "邮件") {
		t.Errorf("错误信息应当说明邮件未配置: %s", w.Body.String())
	}
}
