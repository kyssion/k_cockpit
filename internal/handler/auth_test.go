package handler_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/router"
)

const testPassword = "correct horse battery staple"
const testSecretForHandler = "0123456789abcdef0123456789abcdef"

// newAuthServer 构造带完整路由（含认证中间件）的测试服务。
func newAuthServer(t *testing.T) (*server.Hertz, *gorm.DB) {
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
	router.Register(h, router.Deps{DB: db, Auth: svc, SecureCookie: false})

	return h, db
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

	cookieA := sessionCookie(t, h, "alice")
	cookieB := sessionCookie(t, h, "alice")

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

	// 从 A 撤销 B 的会话。
	w = ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/"+strconv.Itoa(otherID), nil,
		ut.Header{Key: "Cookie", Value: cookieA})
	if w.Code != consts.StatusNoContent {
		t.Fatalf("撤销状态码 = %d, 期望 204 (body=%s)", w.Code, w.Body.String())
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
	cookie := sessionCookie(t, h, "alice")

	w := ut.PerformRequest(h.Engine, "DELETE", "/api/v1/auth/sessions/99999", nil,
		ut.Header{Key: "Cookie", Value: cookie})
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
