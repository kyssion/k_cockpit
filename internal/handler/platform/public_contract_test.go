// Package platform_test 的公开端点字段级契约用例（U-36）。
//
// U-11 的动态用例断言的是**可达性**：15 条公开端点不需要登录即可到达，
// 165 条登录、160 条管理端点一律 401。它回答的是「有没有漏挂守卫」。
//
// 本文件回答另一个问题：**到达之后拿到的是什么**。字段名、类型、可空性、
// 以及「哪些字段刻意不出现」都属于对外契约——前端与集成方按它写代码，
// 改一个 json tag 就会静默破坏调用方，而可达性测试不会有任何反应。
//
// 三条与文档不符的事实，在此以代码为准并已同步更正 DELIVERY_TEST_PLAN：
//   - 邀请预览是 `GET /invites/preview?token=`，不是 `/invites/:token`；
//   - `/version` 挂在 requireAuth 之后，**不是**公开端点；
//   - `/health` 不在 `/api/v1` 下，由 router.Register 直接登记在根路径。
package platform_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"

	"k_cockpit/internal/handler/platform"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/auth"
	"k_cockpit/internal/service/platform/invite"
	"k_cockpit/internal/service/platform/risk"
)

type publicEnv struct {
	h    *server.Hertz
	db   *gorm.DB
	auth *auth.Service
	// inviteSvc 直接用来造邀请，省去「先以管理员身份登录再发邀请」的绕路。
	inviteSvc *invite.Service
}

const publicTestSecret = "public-contract-secret-0123456789"

func newPublicServer(t *testing.T) *publicEnv {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "public.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.User{}, &model.Session{}, &model.AuditLog{}, &model.UserInvite{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	issuer, err := auth.NewTokenIssuer(publicTestSecret)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	authSvc := auth.NewService(db, issuer, recorder, auth.Config{})
	bootstrap, _, err := auth.NewBootstrap(db, recorder)
	if err != nil {
		t.Fatalf("构造引导服务失败: %v", err)
	}
	guard := risk.NewGuard(db, []byte(publicTestSecret), recorder, "")
	inviteSvc := invite.NewService(db, recorder)

	h := server.Default(server.WithDisablePrintRoute(true))
	h.Use(api.RequestID(), api.Recover())

	setupHandler := platform.NewSetup(bootstrap, authSvc, false)
	authHandler := platform.NewAuth(authSvc, false, guard)
	inviteHandler := platform.NewInvite(inviteSvc)

	// /health 在根路径，不在 /api/v1 下（router.Register 直接登记）。
	h.GET("/health", platform.Health(db))

	v1 := h.Group("/api/v1")
	v1.GET("/setup/status", setupHandler.Status)
	v1.POST("/setup/admin", setupHandler.CreateAdmin)
	v1.POST("/auth/login", authHandler.Login)
	v1.POST("/auth/login/verify", authHandler.VerifyLogin)
	v1.POST("/auth/login/password", authHandler.ForceChangePassword)
	v1.POST("/auth/bootstrap/skip", authHandler.SkipBootstrap)
	v1.POST("/auth/bootstrap/totp/setup", authHandler.StagedBeginTOTP)
	v1.POST("/auth/bootstrap/totp/confirm", authHandler.StagedConfirmTOTP)
	v1.POST("/auth/bootstrap/email/code", authHandler.StagedSendEmailCode)
	v1.POST("/auth/bootstrap/email/confirm", authHandler.StagedConfirmEmail)
	v1.POST("/auth/forgot/send", authHandler.RequestPasswordReset)
	v1.POST("/auth/forgot/verify", authHandler.VerifyResetCode)
	v1.POST("/auth/forgot/reset", authHandler.ResetPassword)
	v1.GET("/invites/preview", inviteHandler.Preview)
	v1.POST("/invites/accept", inviteHandler.Accept)

	return &publicEnv{h: h, db: db, auth: authSvc, inviteSvc: inviteSvc}
}

func (e *publicEnv) postJSON(t *testing.T, path, body string) *ut.ResponseRecorder {
	t.Helper()
	return ut.PerformRequest(e.h.Engine, "POST", path,
		&ut.Body{Body: strings.NewReader(body), Len: len(body)},
		ut.Header{Key: "Content-Type", Value: "application/json"})
}

// errorCode 取出统一错误码。
func errorCodeOf(t *testing.T, w *ut.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析错误响应失败: %v (body=%s)", err, w.Body.String())
	}
	return resp.Error.Code
}

// --- GET /setup/status ---

// TestSetupStatusContract 覆盖 `initialized` 的类型与两种取值。
//
// 前端拿这个布尔量决定「显示引导页还是登录页」。如果它被序列化成字符串
// 或者漏掉字段，前端会永远认为系统未初始化——那是一个不需要任何操作就能
// 触发的死循环。
func TestSetupStatusContract(t *testing.T) {
	e := newPublicServer(t)

	// 未初始化：没有任何管理员。
	w := ut.PerformRequest(e.h.Engine, "GET", "/api/v1/setup/status", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			Initialized *bool `json:"initialized"`
		} `json:"data"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Data.Initialized == nil {
		t.Fatal("响应缺少 initialized 字段")
	}
	if *resp.Data.Initialized {
		t.Error("空库时 initialized 应为 false")
	}
	if resp.RequestID == "" {
		t.Error("公开端点也要有 request_id：排障时它是唯一能把日志串起来的线索")
	}

	// 建一个管理员后应为 true。
	hash, err := auth.HashPassword("Some-Password-2026")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if err := e.db.Create(&model.User{
		Username: "admin-x", PasswordHash: hash,
		Role: model.RoleAdmin, Status: model.UserStatusActive,
	}).Error; err != nil {
		t.Fatalf("建管理员失败: %v", err)
	}
	w = ut.PerformRequest(e.h.Engine, "GET", "/api/v1/setup/status", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Data.Initialized == nil || !*resp.Data.Initialized {
		t.Error("已存在管理员时 initialized 应为 true")
	}
}

// --- GET /invites/preview ---

// seedInvite 造一条邀请，返回明文令牌。
//
// 明文只在创建时出现一次（库里只有哈希），所以必须从返回的 link 里取。
func (e *publicEnv) seedInvite(t *testing.T, email string) (token string, view *invite.View) {
	t.Helper()
	view, err := e.inviteSvc.Create(t.Context(), invite.CreateRequest{
		Email: email, Role: model.RoleTenant, TTLHours: 24,
	}, 1, "admin", "127.0.0.1")
	if err != nil {
		t.Fatalf("创建邀请失败: %v", err)
	}
	idx := strings.LastIndex(view.Link, "/")
	if idx < 0 || idx == len(view.Link)-1 {
		t.Fatalf("链接格式异常: %q", view.Link)
	}
	return view.Link[idx+1:], view
}

// TestInvitePreviewContract 覆盖预览的字段集与「刻意不出现」的字段。
//
// 预览面向**任何拿到链接的人**，因此它是这 15 条里最容易泄漏信息的一条：
//   - 不能带配额：那是管理员与受邀人之间的约定；
//   - 不能带 link：链接只在该邀请被创建/重发时返回一次，能预览的人已经
//     有链接了，再回显一遍等于把「拿到链接」变成了「可以转发给别人」；
//   - 不能带 token 或其哈希。
func TestInvitePreviewContract(t *testing.T) {
	e := newPublicServer(t)
	token, created := e.seedInvite(t, "invitee@example.com")

	// 先给这条邀请设一个配额，验证预览真的会把它抹掉。
	if err := e.db.Model(&model.UserInvite{}).Where("id = ?", created.ID).
		Updates(map[string]any{"quota_enabled": true, "quota_bytes": int64(1 << 30)}).Error; err != nil {
		t.Fatalf("写配额失败: %v", err)
	}

	w := ut.PerformRequest(e.h.Engine, "GET", "/api/v1/invites/preview?token="+token, nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	d := resp.Data

	if got := d["email"]; got != "invitee@example.com" {
		t.Errorf("email = %v", got)
	}
	if got := d["role"]; got != model.RoleTenant {
		t.Errorf("role = %v", got)
	}
	if got := d["status"]; got != "pending" {
		t.Errorf("status = %v, 期望 pending", got)
	}
	// expires_at 是 RFC3339 字符串：前端直接 new Date() 用它。
	if s, ok := d["expires_at"].(string); !ok || !strings.Contains(s, "T") {
		t.Errorf("expires_at 应为 RFC3339 字符串, 实际 %#v", d["expires_at"])
	}

	// 配额必须被抹掉。
	if got := d["quota_enabled"]; got != false {
		t.Errorf("预览泄漏了 quota_enabled = %v", got)
	}
	if got := d["quota_bytes"]; got != float64(0) {
		t.Errorf("预览泄漏了 quota_bytes = %v", got)
	}
	// link / token 一个都不该出现。
	for _, forbidden := range []string{"link", "token", "token_hash"} {
		if _, ok := d[forbidden]; ok {
			t.Errorf("预览不该返回 %q 字段: %s", forbidden, w.Body.String())
		}
	}
}

// TestInvitePreviewRejectsBadTokens 覆盖三种无效令牌，且**文案一致**。
//
// 「不存在」「已用」「已撤销」「已过期」对外都是同一句，否则拿到一个乱码
// 令牌的人能靠错误文案区分「这个令牌曾经存在」。
func TestInvitePreviewRejectsBadTokens(t *testing.T) {
	e := newPublicServer(t)

	// 缺 token：这是调用方的用法错误，可以明说。
	w := ut.PerformRequest(e.h.Engine, "GET", "/api/v1/invites/preview", nil)
	if w.Code != consts.StatusBadRequest {
		t.Errorf("缺 token 状态码 = %d, 期望 400", w.Code)
	}

	// 不存在的令牌。
	w = ut.PerformRequest(e.h.Engine, "GET", "/api/v1/invites/preview?token=deadbeef", nil)
	if w.Code != consts.StatusNotFound {
		t.Errorf("未知令牌状态码 = %d, 期望 404", w.Code)
	}
	unknownMsg := errorCodeOf(t, w)

	// 已接受。
	token, created := e.seedInvite(t, "used@example.com")
	if err := e.db.Model(&model.UserInvite{}).Where("id = ?", created.ID).
		Update("accepted_at", "2026-01-01T00:00:00Z").Error; err != nil {
		t.Fatalf("标记已接受失败: %v", err)
	}
	w = ut.PerformRequest(e.h.Engine, "GET", "/api/v1/invites/preview?token="+token, nil)
	if w.Code != consts.StatusNotFound {
		t.Errorf("已接受令牌状态码 = %d, 期望 404", w.Code)
	}
	// 错误码相同即可（文案本身允许微调，但语义分类不能变）。
	if got := errorCodeOf(t, w); got != unknownMsg {
		t.Errorf("已接受与未知令牌的错误码不同: %q vs %q", got, unknownMsg)
	}

	// 已过期。
	token2, created2 := e.seedInvite(t, "expired@example.com")
	if err := e.db.Model(&model.UserInvite{}).Where("id = ?", created2.ID).
		Update("expires_at", "2020-01-01T00:00:00Z").Error; err != nil {
		t.Fatalf("标记过期失败: %v", err)
	}
	w = ut.PerformRequest(e.h.Engine, "GET", "/api/v1/invites/preview?token="+token2, nil)
	if w.Code != consts.StatusNotFound {
		t.Errorf("过期令牌状态码 = %d, 期望 404", w.Code)
	}
}

// --- POST /invites/accept ---

// TestInviteAcceptContract 覆盖接受邀请的响应形态。
//
// 成功后**不建会话**（返回 204 而不是 200 + Cookie）：注册完需要自己登录
// 一次，那也是一次「他知道自己的密码」的确认。这条契约若被改成自动登录，
// 前端会在注册后直接进入已登录态，而用户从没输入过密码。
func TestInviteAcceptContract(t *testing.T) {
	e := newPublicServer(t)

	var createdUser string
	e.inviteSvc.SetUserCreator(func(
		_ context.Context, email, username, password, role string, _ bool, _ int64,
	) error {
		createdUser = username
		return nil
	})

	token, _ := e.seedInvite(t, "accept@example.com")
	w := e.postJSON(t, "/api/v1/invites/accept",
		`{"token":"`+token+`","username":"newbie","password":"Long-Enough-Pass-1"}`)

	if w.Code != consts.StatusNoContent {
		t.Fatalf("状态码 = %d, 期望 204 (body=%s)", w.Code, w.Body.String())
	}
	if createdUser != "newbie" {
		t.Errorf("账号创建回调收到的用户名 = %q", createdUser)
	}
	// 不建会话：没有 Set-Cookie。
	if got := string(w.Header().Peek("Set-Cookie")); got != "" {
		t.Errorf("接受邀请不该下发会话 Cookie: %q", got)
	}

	// 缺少 token：400，而不是 500。
	w = e.postJSON(t, "/api/v1/invites/accept", `{"username":"x","password":"Long-Enough-Pass-1"}`)
	if w.Code != consts.StatusBadRequest {
		t.Errorf("缺 token 状态码 = %d, 期望 400", w.Code)
	}
}

// --- POST /auth/login ---

// TestLoginContractDoesNotRevealUserExistence 覆盖统一失败文案（R-002）。
//
// 「用户名不存在」与「密码错误」必须返回**完全相同**的状态码与错误码。
// 差异会让任何人都能批量探测哪些用户名存在——那是最基础的账号枚举。
func TestLoginContractDoesNotRevealUserExistence(t *testing.T) {
	e := newPublicServer(t)

	// 造一个真实账号。
	hash, err := auth.HashPassword("Real-Password-2026")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if err := e.db.Create(&model.User{
		Username: "real-user", PasswordHash: hash,
		Role: model.RoleTenant, Status: model.UserStatusActive,
	}).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	wrongPass := e.postJSON(t, "/api/v1/auth/login",
		`{"username":"real-user","password":"wrong-password"}`)
	noSuchUser := e.postJSON(t, "/api/v1/auth/login",
		`{"username":"nobody-here","password":"wrong-password"}`)

	if wrongPass.Code != consts.StatusUnauthorized || noSuchUser.Code != consts.StatusUnauthorized {
		t.Fatalf("状态码 = %d / %d, 期望都是 401",
			wrongPass.Code, noSuchUser.Code)
	}

	var a, b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(wrongPass.Body.Bytes(), &a)
	_ = json.Unmarshal(noSuchUser.Body.Bytes(), &b)
	if a.Error.Code != b.Error.Code || a.Error.Message != b.Error.Message {
		t.Errorf("失败文案不一致，可用于枚举用户名:\n  密码错: %+v\n  无此人: %+v", a.Error, b.Error)
	}
	// 响应体里不该出现「不存在」「未找到」这类词。
	for _, word := range []string{"不存在", "未找到", "没有该用户"} {
		if strings.Contains(wrongPass.Body.String(), word) ||
			strings.Contains(noSuchUser.Body.String(), word) {
			t.Errorf("失败文案含 %q，泄漏了账号是否存在: %s", word, noSuchUser.Body.String())
		}
	}

	// 缺字段：400 而不是 401——调用方少传参数不该被当成凭据错误。
	w := e.postJSON(t, "/api/v1/auth/login", `{"username":"real-user"}`)
	if w.Code != consts.StatusBadRequest {
		t.Errorf("缺 password 状态码 = %d, 期望 400", w.Code)
	}
}

// TestLoginSuccessShape 覆盖成功响应的字段与「刻意不出现」的令牌。
//
// R-014：令牌与 session_id 不得出现在响应体中。完整登录的令牌由
// HttpOnly Cookie 承载（XSS 读不到），响应体只给 stage / user / expires_at。
// 这条契约反过来说也成立：**如果有人把 token 加回响应体，这里会失败**——
// 那是一次不需要前端配合就能生效的降级。
func TestLoginSuccessShape(t *testing.T) {
	e := newPublicServer(t)
	hash, err := auth.HashPassword("Real-Password-2026")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if err := e.db.Create(&model.User{
		Username: "login-ok", PasswordHash: hash,
		Role: model.RoleTenant, Status: model.UserStatusActive,
	}).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	w := e.postJSON(t, "/api/v1/auth/login",
		`{"username":"login-ok","password":"Real-Password-2026"}`)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	// 令牌在 Cookie 里，且必须是 HttpOnly。
	cookie := string(w.Header().Peek("Set-Cookie"))
	if !strings.Contains(cookie, "kc_session=") {
		t.Fatalf("登录成功应下发会话 Cookie，实际 %q", cookie)
	}
	if !strings.Contains(strings.ToLower(cookie), "httponly") {
		t.Errorf("会话 Cookie 必须是 HttpOnly（否则 XSS 能直接读走）: %q", cookie)
	}

	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	d := resp.Data

	if got := d["stage"]; got != string(auth.StageOK) {
		t.Errorf("stage = %v, 期望 %q", got, auth.StageOK)
	}
	if _, ok := d["expires_at"]; !ok {
		t.Error("响应缺少 expires_at")
	}
	user, ok := d["user"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 user 对象: %s", w.Body.String())
	}
	if user["username"] != "login-ok" {
		t.Errorf("user.username = %v", user["username"])
	}

	// R-014：响应体不得回显令牌。
	for _, forbidden := range []string{"token", "login_token", "access_token", "session_id"} {
		if _, ok := d[forbidden]; ok {
			t.Errorf("响应体不该出现 %q（R-014：令牌与会话标识只经 Cookie 传递）", forbidden)
		}
	}
	// 用户视图不得带安全字段。
	for _, forbidden := range []string{"password_hash", "totp_secret_enc", "recovery_codes_hash"} {
		if _, ok := user[forbidden]; ok {
			t.Errorf("user 里不该出现 %q", forbidden)
		}
	}
}

// --- 全部公开端点的「不得 500」契约 ---

// TestPublicEndpointsNeverFailWith500 覆盖 15 条公开端点的缺参数行为。
//
// 公开端点没有登录态可依赖，参数缺失是**常态**（爬虫、错误的前端版本、
// 用户手改 URL）。这里的分界线是 **500**：`INTERNAL_ERROR` 表示「我们没想
// 到这种情况」，它在监控上就是一次故障，且文案是「请稍后重试」——而缺参数
// 与装配缺失都不会因为重试而变好。
//
// 503 是例外且必须带可执行的说明（邮件未配置、邀请注册未启用）：它是
// 「能力未开启」的诚实表达，不是意外。
func TestPublicEndpointsNeverFailWith500(t *testing.T) {
	e := newPublicServer(t)

	posts := []string{
		"/api/v1/setup/admin",
		"/api/v1/auth/login",
		"/api/v1/auth/login/verify",
		"/api/v1/auth/login/password",
		"/api/v1/auth/bootstrap/skip",
		"/api/v1/auth/bootstrap/totp/setup",
		"/api/v1/auth/bootstrap/totp/confirm",
		"/api/v1/auth/bootstrap/email/code",
		"/api/v1/auth/bootstrap/email/confirm",
		"/api/v1/auth/forgot/send",
		"/api/v1/auth/forgot/verify",
		"/api/v1/auth/forgot/reset",
		"/api/v1/invites/accept",
	}
	for _, path := range posts {
		// 空对象：所有字段都缺。
		w := e.postJSON(t, path, `{}`)
		if w.Code == consts.StatusInternalServerError {
			t.Errorf("POST %s 空参数返回 500（INTERNAL_ERROR 会被当成故障并诱导重试）: %s",
				path, w.Body.String())
		}
		if w.Code == consts.StatusServiceUnavailable {
			// 允许 503，但必须说明原因，不能是泛化文案。
			if strings.Contains(w.Body.String(), "服务内部错误") {
				t.Errorf("POST %s 返回 503 但文案是泛化错误: %s", path, w.Body.String())
			}
		}
		// 垃圾体：不是合法 JSON。
		w = e.postJSON(t, path, `not-json`)
		if w.Code != consts.StatusBadRequest {
			t.Errorf("POST %s 非法 JSON 返回 %d, 期望 400", path, w.Code)
		}
	}

	// 两条 GET 也不能 5xx。
	for _, path := range []string{"/api/v1/setup/status", "/api/v1/invites/preview"} {
		w := ut.PerformRequest(e.h.Engine, "GET", path, nil)
		if w.Code >= 500 {
			t.Errorf("GET %s 返回 %d: %s", path, w.Code, w.Body.String())
		}
	}
	// /health 在根路径，且必须始终可用——它是负载均衡摘流量的唯一依据。
	w := ut.PerformRequest(e.h.Engine, "GET", "/health", nil)
	if w.Code != consts.StatusOK {
		t.Errorf("/health 状态码 = %d, 期望 200", w.Code)
	}
}

// TestForgotPasswordDoesNotRevealAccount 覆盖找回密码的防枚举。
//
// 不论邮箱是否绑定过账号，都必须返回同一种结果：差异会让「这个邮箱有没有
// 注册过」变成一个可以批量查询的事实。
func TestForgotPasswordDoesNotRevealAccount(t *testing.T) {
	e := newPublicServer(t)

	hash, _ := auth.HashPassword("Real-Password-2026")
	email := "known@example.com"
	if err := e.db.Create(&model.User{
		Username: "has-email", PasswordHash: hash,
		Role: model.RoleTenant, Status: model.UserStatusActive, Email: &email,
	}).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	known := e.postJSON(t, "/api/v1/auth/forgot/send", `{"email":"known@example.com"}`)
	unknown := e.postJSON(t, "/api/v1/auth/forgot/send", `{"email":"nobody@example.com"}`)

	// 未装配邮件服务时两者都应是「尚未配置」（503）。关键不在具体码，
	// 而在**两者一致**，且不能是泛化的 500「请稍后重试」。
	if known.Code != unknown.Code {
		t.Errorf("已知邮箱返回 %d、未知邮箱返回 %d —— 差异可用于枚举账号",
			known.Code, unknown.Code)
	}
	if known.Code == consts.StatusInternalServerError {
		t.Errorf("找回密码返回 500；未配置邮件是能力缺失，应明确说明")
	}
}
