package handler_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/pquerna/otp/totp"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/router"
)

const testPassword = "correct horse battery staple"
const testSecretForHandler = "0123456789abcdef0123456789abcdef"

// newAuthServer 构造带完整路由（含认证中间件）的测试服务。
func newAuthServer(t *testing.T) (*server.Hertz, *gorm.DB) {
	t.Helper()
	h, db, _ := newAuthServerWithGuard(t)
	return h, db
}

// newAuthServerWithGuard 额外返回验证守卫，供需要走完验证流程的测试使用。
func newAuthServerWithGuard(t *testing.T) (*server.Hertz, *gorm.DB, *risk.Guard) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "auth_handler.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	issuer, err := auth.NewTokenIssuer(testSecretForHandler)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	svc := auth.NewService(db, issuer, audit.NewRecorder(db), auth.Config{})

	h := server.Default()
	// guard **必须**接入：nil 会让受保护的操作在调用时 panic。这是刻意的
	// 快速失败——生产环境漏接 guard 等于全部高风险操作失去保护，而它不会
	// 以任何显式方式暴露出来。
	guard := risk.NewGuard(db, []byte(testSecretForHandler), audit.NewRecorder(db), "")
	router.Register(h, router.Deps{DB: db, Auth: svc, Risk: guard, SecureCookie: false})

	return h, db, guard
}

// bindTOTP 为账号绑定验证器，返回绑定后的会话与密钥。
//
// 刻意不绕过绑定（例如直接往库里写一个密钥）：绑定、算码、验证这条链路上
// 任一环节坏掉都应当被测试发现——绕过去的话，测出来的是一个用户永远不会
// 经历的路径。
//
// 返回的 cookie 是**绑定完成后重新登录**得到的：2FA 变更会让全部既有会话
// 失效（f-1-01 R-010），拿着绑定前的 cookie 继续请求会得到 401。
func bindTOTP(t *testing.T, h *server.Hertz, username string) (cookie, secret string) {
	t.Helper()

	cookie = sessionCookie(t, h, username)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/totp/setup", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if w.Code != consts.StatusOK {
		t.Fatalf("开始绑定状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	var setupBody struct {
		Data struct {
			OTPAuthURI string `json:"otpauth_uri"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &setupBody); err != nil {
		t.Fatalf("解析绑定响应失败: %v", err)
	}

	secret = secretFromOTPAuthURI(t, setupBody.Data.OTPAuthURI)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("计算动态码失败: %v", err)
	}

	w = ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/totp/confirm",
		jsonBody(map[string]string{"code": code}),
		ut.Header{Key: "Cookie", Value: cookie},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusOK {
		t.Fatalf("确认绑定状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	// 安全信息已变更，重新登录拿到有效会话。
	return sessionCookie(t, h, username), secret
}

// issueRiskGrant 完成一次「触发 428 → 提交验证码 → 换取许可」的完整流程。
func issueRiskGrant(t *testing.T, h *server.Hertz, cookie, secret string) string {
	t.Helper()

	// 触发一次受保护操作，拿到 challenge。
	w := ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/999999", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("触发 428 失败, 状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	var required struct {
		Data struct {
			ChallengeID string `json:"challenge_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &required); err != nil {
		t.Fatalf("解析 428 响应失败: %v", err)
	}

	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("计算动态码失败: %v", err)
	}

	w = ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/risk-verification",
		jsonBody(map[string]string{
			"challenge_id": required.Data.ChallengeID,
			"method":       string(risk.MethodTOTP),
			"code":         code,
		}),
		ut.Header{Key: "Cookie", Value: cookie},
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusOK {
		t.Fatalf("提交验证码状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	var granted struct {
		Data struct {
			Grant string `json:"grant"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &granted); err != nil {
		t.Fatalf("解析许可响应失败: %v", err)
	}
	if granted.Data.Grant == "" {
		t.Fatal("验证通过但未返回许可")
	}
	return granted.Data.Grant
}

// clearSessions 清空会话记录。
//
// 用于把「绑定流程留下的历史会话」与测试真正关注的内容分开：会话列表按
// 设计包含已失效的登录记录（供审计追溯），而多数测试只关心当前有效的几个。
func clearSessions(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Where("1 = 1").Delete(&model.Session{}).Error; err != nil {
		t.Fatalf("清理会话记录失败: %v", err)
	}
}

// secretFromOTPAuthURI 从 otpauth:// 链接中取出密钥。
func secretFromOTPAuthURI(t *testing.T, uri string) string {
	t.Helper()

	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("解析 otpauth URI 失败: %v", err)
	}
	secret := parsed.Query().Get("secret")
	if secret == "" {
		t.Fatalf("otpauth URI 中缺少 secret: %s", uri)
	}
	return secret
}

// TestRevokeSessionRequiresRiskVerification 覆盖高风险操作的 428 流程。
//
// 撤销会话属于「影响可达性」的操作（f-10-02），未完成二次验证时必须被拦下
// 而不是执行——这里验证拦截本身，以及响应里带了什么让前端能继续。
func TestRevokeSessionRequiresRiskVerification(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleTenant)
	cookie := sessionCookie(t, h, "alice")

	w := ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/1", nil,
		ut.Header{Key: "Cookie", Value: cookie})

	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("未验证的高风险操作状态码 = %d, 期望 428 (body=%s)", w.Code, w.Body.String())
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Data struct {
			Action      string `json:"action"`
			ChallengeID string `json:"challenge_id"`
			Methods     []any  `json:"methods"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 428 响应失败: %v", err)
	}
	if body.Error.Code != api.CodeRiskVerification {
		t.Errorf("错误码 = %q, 期望 %q", body.Error.Code, api.CodeRiskVerification)
	}
	// 前端要靠这两项才能发起验证：challenge 标识这次流程，methods 决定
	// 弹框渲染哪种输入方式（前端不得硬编码）。
	if body.Data.ChallengeID == "" {
		t.Error("428 响应缺少 challenge_id，前端无法发起验证")
	}
	if body.Data.Action != string(risk.ActionSessionRevoke) {
		t.Errorf("action = %q, 期望 %q", body.Data.Action, risk.ActionSessionRevoke)
	}
	// 未绑定任何验证方式时 methods 为空数组而非 null：前端据此提示
	// 「需先绑定」，而不是渲染一个没有选项的空弹框。
	if body.Data.Methods == nil {
		t.Error("methods 不应为 null")
	}
}

func seedUser(t *testing.T, db *gorm.DB, username, role string) int64 {
	t.Helper()

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	u := model.User{Username: username, PasswordHash: hash, Role: role, Status: model.UserStatusActive}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	return u.ID
}

func loginBody(username, password string) *ut.Body {
	b, _ := json.Marshal(map[string]string{"username": username, "password": password})
	return &ut.Body{Body: strings.NewReader(string(b)), Len: len(b)}
}

// jsonBody 构造 JSON 请求体。
func jsonBody(v any) *ut.Body {
	b, _ := json.Marshal(v)
	return &ut.Body{Body: strings.NewReader(string(b)), Len: len(b)}
}

// sessionCookie 执行登录并返回可回传的 Cookie 头值。
func sessionCookie(t *testing.T, h *server.Hertz, username string) string {
	t.Helper()

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/login", loginBody(username, testPassword),
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusOK {
		t.Fatalf("登录失败: status=%d body=%s", w.Code, w.Body.String())
	}

	raw := w.Header().Get("Set-Cookie")
	if raw == "" {
		t.Fatal("登录响应未下发 Set-Cookie")
	}
	token := tokenFromSetCookie(t, raw)
	return auth.CookieName + "=" + token
}

func TestLoginSetsHardenedCookie(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleTenant)

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/login", loginBody("alice", testPassword),
		ut.Header{Key: "Content-Type", Value: "application/json"})

	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200 (body=%s)", w.Code, w.Body.String())
	}

	// Cookie 属性名大小写不敏感，统一转小写后比较。
	raw := strings.ToLower(w.Header().Get("Set-Cookie"))
	for _, want := range []string{"httponly", "samesite=strict", "path=/"} {
		if !strings.Contains(raw, want) {
			t.Errorf("Cookie 缺少 %q 属性: %s", want, raw)
		}
	}

	// R-014：令牌不得出现在响应体中，前端 JS 不应接触它。
	token := tokenFromSetCookie(t, raw)
	if strings.Contains(w.Body.String(), token) {
		t.Error("响应体中出现了令牌——令牌应只经 HttpOnly Cookie 下发")
	}

	out := decodeBody(t, w.Body.Bytes())
	data, _ := out["data"].(map[string]any)
	user, _ := data["user"].(map[string]any)
	if user["username"] != "alice" {
		t.Errorf("返回用户 = %v, 期望 username=alice", user)
	}
	// 只返回基本信息，不含安全字段与配额明细。
	for _, forbidden := range []string{"password", "totp", "recovery", "max_vm", "security_updated_at"} {
		if _, ok := user[forbidden]; ok {
			t.Errorf("用户信息中不应包含字段 %q", forbidden)
		}
	}
}

func TestLoginFailureIsUniform(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleTenant)

	cases := map[string]struct{ user, pw string }{
		"用户不存在": {"nobody", testPassword},
		"密码错误":  {"alice", "wrong"},
	}

	var first string
	for name, tc := range cases {
		w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/login", loginBody(tc.user, tc.pw),
			ut.Header{Key: "Content-Type", Value: "application/json"})

		if w.Code != consts.StatusUnauthorized {
			t.Errorf("%s: 状态码 = %d, 期望 401", name, w.Code)
		}
		if w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s: 登录失败不应下发 Cookie", name)
		}

		msg := errorMessage(t, w.Body.Bytes())
		if first == "" {
			first = msg
			continue
		}
		if msg != first {
			t.Errorf("%s 的错误文案与首个失败不一致: %q vs %q", name, msg, first)
		}
	}
}

func TestSessionRequiresAuth(t *testing.T) {
	h, _ := newAuthServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil)
	if w.Code != consts.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401", w.Code)
	}
	if code := errorCode(t, w.Body.Bytes()); code != api.CodeUnauthenticated {
		t.Errorf("错误码 = %q, 期望 %q", code, api.CodeUnauthenticated)
	}
}

func TestSessionWithCookie(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleAdmin)
	cookie := sessionCookie(t, h, "alice")

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil,
		ut.Header{Key: "Cookie", Value: cookie})

	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200 (body=%s)", w.Code, w.Body.String())
	}

	out := decodeBody(t, w.Body.Bytes())
	data, _ := out["data"].(map[string]any)
	user, _ := data["user"].(map[string]any)
	if user["role"] != model.RoleAdmin {
		t.Errorf("角色 = %v, 期望 admin", user["role"])
	}

	session, _ := data["session"].(map[string]any)
	if session["current"] != true {
		t.Error("当前会话未标记 current=true")
	}
	// R-014：响应中不得出现 session_id。
	for _, forbidden := range []string{"session_id", "token"} {
		if _, ok := session[forbidden]; ok {
			t.Errorf("会话信息中不应包含字段 %q", forbidden)
		}
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleTenant)
	cookie := sessionCookie(t, h, "alice")

	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/logout", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if w.Code != consts.StatusNoContent {
		t.Fatalf("登出状态码 = %d, 期望 204", w.Code)
	}
	// 登出必须清除浏览器 Cookie，否则会带着失效令牌反复 401。
	if sc := strings.ToLower(w.Header().Get("Set-Cookie")); !strings.Contains(sc, "max-age=0") {
		t.Errorf("登出未清除 Cookie: %s", sc)
	}

	// 同一个 Cookie 再次访问必须被拒绝（令牌签名仍有效，但会话已撤销）。
	w = ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if w.Code != consts.StatusUnauthorized {
		t.Errorf("登出后仍可访问, 状态码 = %d", w.Code)
	}
}

func TestSessionsListAndRevoke(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleTenant)

	// 先绑定验证器，再清掉绑定流程留下的会话记录，专注于本测试关注的会话。
	_, secret := bindTOTP(t, h, "alice")
	clearSessions(t, db)

	cookieA := sessionCookie(t, h, "alice")
	cookieB := sessionCookie(t, h, "alice")

	// 撤销会话是高风险操作，必须先通过二次验证（f-10-01）。
	grant := issueRiskGrant(t, h, cookieA, secret)

	// 两个设备各一个会话，列表里都能看到，且只有一个是 current。
	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/sessions", nil,
		ut.Header{Key: "Cookie", Value: cookieA})
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", w.Code)
	}

	out := decodeBody(t, w.Body.Bytes())
	list, _ := out["data"].([]any)
	if len(list) != 2 {
		t.Fatalf("会话数 = %d, 期望 2", len(list))
	}

	var currentCount, otherID int
	for _, item := range list {
		s, _ := item.(map[string]any)
		if s["current"] == true {
			currentCount++
			continue
		}
		otherID = int(s["id"].(float64))
	}
	if currentCount != 1 {
		t.Errorf("current 标记数量 = %d, 期望 1", currentCount)
	}

	// 从 A 撤销 B 的会话，携带许可重放。
	w = ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/"+strconv.Itoa(otherID), nil,
		ut.Header{Key: "Cookie", Value: cookieA},
		ut.Header{Key: risk.GrantHeader, Value: grant})
	if w.Code != consts.StatusNoContent {
		t.Fatalf("撤销状态码 = %d, 期望 204 (body=%s)", w.Code, w.Body.String())
	}

	// 许可是一次性的：同一令牌再用一次必须被拒（Q-004）。
	//
	// 这是整套机制里最关键的一条——若许可可重用，「验证一次、连续删多次」
	// 就会成为现实，二次确认的意义随之消失。
	w = ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/"+strconv.Itoa(otherID), nil,
		ut.Header{Key: "Cookie", Value: cookieA},
		ut.Header{Key: risk.GrantHeader, Value: grant})
	if w.Code != consts.StatusPreconditionRequired {
		t.Errorf("重放已消费的许可状态码 = %d, 期望 428", w.Code)
	}

	// B 的会话立即失效，A 不受影响。
	if w := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil,
		ut.Header{Key: "Cookie", Value: cookieB}); w.Code != consts.StatusUnauthorized {
		t.Errorf("被撤销的会话仍可访问, 状态码 = %d", w.Code)
	}
	if w := ut.PerformRequest(h.Engine, "GET", "/api/v1/auth/session", nil,
		ut.Header{Key: "Cookie", Value: cookieA}); w.Code != consts.StatusOK {
		t.Errorf("当前会话不应受影响, 状态码 = %d", w.Code)
	}
}

// 撤销不存在的会话返回 404，与「无权访问他人会话」表现一致。
func TestRevokeUnknownSessionReturns404(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "alice", model.RoleTenant)

	_, secret := bindTOTP(t, h, "alice")
	cookie := sessionCookie(t, h, "alice")
	grant := issueRiskGrant(t, h, cookie, secret)

	w := ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/99999", nil,
		ut.Header{Key: "Cookie", Value: cookie},
		ut.Header{Key: risk.GrantHeader, Value: grant})
	if w.Code != consts.StatusNotFound {
		t.Errorf("状态码 = %d, 期望 404", w.Code)
	}
}

func TestLoginRejectsEmptyCredentials(t *testing.T) {
	h, _ := newAuthServer(t)

	for name, body := range map[string]*ut.Body{
		"用户名空":  loginBody("", testPassword),
		"密码空":   loginBody("alice", ""),
		"请求体非法": {Body: strings.NewReader("{not json"), Len: 9},
	} {
		w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/login", body,
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if w.Code != consts.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d, 期望 400", name, w.Code)
		}
	}
}

func tokenFromSetCookie(t *testing.T, raw string) string {
	t.Helper()
	cookie, err := http.ParseSetCookie(raw)
	if err != nil {
		t.Fatalf("解析 Set-Cookie 失败: %v (%s)", err, raw)
	}
	if cookie.Name != auth.CookieName || cookie.Value == "" {
		t.Fatalf("Set-Cookie 内容异常: %s", raw)
	}
	return cookie.Value
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, body)
	}
	return out
}

func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	out := decodeBody(t, body)
	e, _ := out["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	out := decodeBody(t, body)
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}
