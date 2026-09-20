package handler

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
	"k_cockpit/internal/risk"
)

// Auth 提供认证相关接口。
type Auth struct {
	svc          *auth.Service
	secureCookie bool
	risk         *risk.Guard
}

// NewAuth 构造认证接口。
//
// secureCookie 在 HTTPS 部署时必须为 true；本地 HTTP 开发必须为 false，
// 否则浏览器不会回传带 Secure 标记的 Cookie，表现为「登录成功但仍未认证」。
func NewAuth(svc *auth.Service, secureCookie bool, guard *risk.Guard) *Auth {
	return &Auth{svc: svc, secureCookie: secureCookie, risk: guard}
}

// userView 是返回给前端的用户信息。
//
// 按 f-1-01 §5.2，只含基本信息：**不含**配额明细与安全字段
// （如 totp_enabled、recovery_codes_hash），它们不应离开服务端。
type userView struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// sessionView 是返回给前端的会话信息。
//
// **不含 session_id 与令牌**（R-014：令牌与 session_id 不得出现在响应中）。
// 前端只需 id 用于撤销，以及展示用的设备与时间信息。
type sessionView struct {
	ID           int64      `json:"id"`
	ClientIP     string     `json:"client_ip,omitempty"`
	UserAgent    string     `json:"user_agent,omitempty"`
	IssuedAt     time.Time  `json:"issued_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	Current      bool       `json:"current"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	// Stage 告诉前端下一步做什么。
	//
	// 登录成功不再等于"拿到会话"：强制改密、二次验证与安全初始化都要在
	// 这一步之后继续。让前端自己拿 totp_enabled / force_password_change
	// 去推，等于把这套判定复制一份到浏览器里，而它迟早与服务端分叉。
	Stage Stage `json:"stage"`
	// LoginToken 只在未完成阶段返回。它**不下发 Cookie**：中间态令牌的
	// 寿命是五分钟，放进 Cookie 会让"卡在改密这一步"的状态长期驻留。
	LoginToken string `json:"login_token,omitempty"`
	// Methods 是二次验证可选方式，供前端渲染输入框提示。
	Methods   []string   `json:"methods,omitempty"`
	User      *userView  `json:"user,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Stage 与 auth.Stage 同构，在此重新声明是为了让响应结构的 JSON 契约
// 保持在接口层可见，而不必让前端去猜字符串取值。
type Stage = string

// Login 处理用户名密码登录。
//
// 公开接口（无需认证）：这是获取凭据的入口，必须显式标记为公开。
func (h *Auth) Login(ctx context.Context, c *app.RequestContext) {
	var req loginRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.Username == "" || req.Password == "" {
		api.Fail(c, api.InvalidParameter("用户名与密码不能为空"))
		return
	}

	res, err := h.svc.Login(ctx, req.Username, req.Password, auth.ClientInfoOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	if res.Stage == auth.StageOK {
		setSessionCookie(c, res.Token, res.ExpiresAt, h.secureCookie)
		view := userViewOf(res.User)
		api.OK(c, loginResponse{Stage: string(auth.StageOK), User: &view, ExpiresAt: &res.ExpiresAt})
		return
	}

	// 未完成阶段：只给中间态令牌，不下发 Cookie。
	api.OK(c, loginResponse{
		Stage:      string(res.Stage),
		LoginToken: res.Token,
		ExpiresAt:  &res.ExpiresAt,
	})
}

// --- 登录后续阶段（F-1-08）---
//
// 这一组接口的共同点：它们用**请求体里的 login_token** 认证，而不是 Cookie。
// 中间态令牌只有五分钟且不进 Cookie，因此这几步只能在一次连续的登录流程
// 里完成——刷新页面就得重新登录，这是刻意的（见 AuthenticateStage）。

type verifyLoginRequest struct {
	LoginToken string `json:"login_token"`
	Code       string `json:"code"`
}

// VerifyLogin 完成登录阶段的二次验证。
func (h *Auth) VerifyLogin(ctx context.Context, c *app.RequestContext) {
	var req verifyLoginRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.LoginToken == "" || req.Code == "" {
		api.Fail(c, api.InvalidParameter("请填写验证码"))
		return
	}
	res, err := h.svc.VerifyLogin(ctx, req.LoginToken, req.Code, auth.ClientInfoOf(c))
	completeLogin(c, res, err, h.secureCookie)
}

type forceChangeRequest struct {
	LoginToken  string `json:"login_token"`
	NewPassword string `json:"new_password"`
}

// ForceChangePassword 完成强制改密。
func (h *Auth) ForceChangePassword(ctx context.Context, c *app.RequestContext) {
	var req forceChangeRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.LoginToken == "" || req.NewPassword == "" {
		api.Fail(c, api.InvalidParameter("请填写新密码"))
		return
	}
	res, err := h.svc.ForceChangePassword(ctx, req.LoginToken, req.NewPassword, auth.ClientInfoOf(c))
	completeLogin(c, res, err, h.secureCookie)
}

type bootstrapRequest struct {
	LoginToken string `json:"login_token"`
}

// CompleteBootstrap 清除"已跳过安全初始化"标记（已登录用户）。
func (h *Auth) CompleteBootstrap(ctx context.Context, c *app.RequestContext) {
	if err := h.svc.CompleteBootstrap(ctx, auth.CurrentUser(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

type stagedTOTPRequest struct {
	LoginToken string `json:"login_token"`
	Code       string `json:"code"`
}

// StagedBeginTOTP 在引导阶段开始绑定验证器。
//
// 引导期的用户只有中间态令牌，进不了要求访问级会话的 /auth/totp/*；
// 但"绑定邮箱 + 启用 2FA"正是引导要他做的事——没有这两条入口，引导页
// 就是一个只能点「跳过」的死胡同。
func (h *Auth) StagedBeginTOTP(ctx context.Context, c *app.RequestContext) {
	var req stagedTOTPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user, _, err := h.svc.AuthenticateStage(ctx, req.LoginToken, model.TokenTypeLogin, auth.ClientInfoOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	uri, err := h.risk.BeginTOTPSetup(ctx, user)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"otpauth_uri": uri})
}

// StagedConfirmTOTP 在引导阶段确认绑定。
//
// 绑定会让既有会话失效（安全信息变更 R-010），因此这一步之后引导页要
// 重新登录——那时 2FA 已生效，登录会停在 login_verify，用户用刚绑好的
// 验证器输入动态码即可进入系统。
func (h *Auth) StagedConfirmTOTP(ctx context.Context, c *app.RequestContext) {
	var req stagedTOTPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user, _, err := h.svc.AuthenticateStage(ctx, req.LoginToken, model.TokenTypeLogin, auth.ClientInfoOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	codes, err := h.risk.ConfirmTOTPSetup(ctx, user, req.Code)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"recovery_codes": codes})
}

// SkipBootstrap 跳过安全初始化引导。
func (h *Auth) SkipBootstrap(ctx context.Context, c *app.RequestContext) {
	var req bootstrapRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	res, err := h.svc.SkipBootstrap(ctx, req.LoginToken, auth.ClientInfoOf(c))
	completeLogin(c, res, err, h.secureCookie)
}

type stagedEmailRequest struct {
	LoginToken string `json:"login_token"`
	Email      string `json:"email"`
	Code       string `json:"code"`
}

// StagedSendEmailCode 在引导阶段发送邮箱绑定验证码。
func (h *Auth) StagedSendEmailCode(ctx context.Context, c *app.RequestContext) {
	var req stagedEmailRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.SendStagedEmailCode(ctx, req.LoginToken, req.Email, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

// StagedConfirmEmail 在引导阶段确认绑定邮箱。
func (h *Auth) StagedConfirmEmail(ctx context.Context, c *app.RequestContext) {
	var req stagedEmailRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.ConfirmStagedEmail(ctx, req.LoginToken, req.Email, req.Code, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

// completeLogin 统一结束一个登录阶段：成功后下发访问会话 Cookie。
func completeLogin(c *app.RequestContext, res *auth.LoginResult, err error, secure bool) {
	if err != nil {
		api.Fail(c, err)
		return
	}
	setSessionCookie(c, res.Token, res.ExpiresAt, secure)
	view := userViewOf(res.User)
	api.OK(c, loginResponse{Stage: string(res.Stage), User: &view, ExpiresAt: &res.ExpiresAt})
}

// --- 邮箱绑定（已登录）---

type emailRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// SendEmailCode 向指定邮箱发送绑定验证码。
func (h *Auth) SendEmailCode(ctx context.Context, c *app.RequestContext) {
	var req emailRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.SendEmailCode(ctx, auth.CurrentUser(c), req.Email, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

// ConfirmEmail 校验验证码并绑定邮箱。
func (h *Auth) ConfirmEmail(ctx context.Context, c *app.RequestContext) {
	var req emailRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.ConfirmEmail(ctx, auth.CurrentUser(c), req.Email, req.Code); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

// --- 找回密码（公开）---

type forgotRequest struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

// RequestPasswordReset 发起找回密码。
//
// 无论邮箱是否存在都返回成功：否则它就是"某个人是不是本面板用户"的
// 查询入口。
func (h *Auth) RequestPasswordReset(ctx context.Context, c *app.RequestContext) {
	var req forgotRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.RequestPasswordReset(ctx, req.Email, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

// VerifyResetCode 校验邮件验证码，换发重置票据。
func (h *Auth) VerifyResetCode(ctx context.Context, c *app.RequestContext) {
	var req forgotRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	token, err := h.svc.VerifyResetCode(ctx, req.Email, req.Code, auth.ClientInfoOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"reset_token": token})
}

type resetPasswordRequest struct {
	ResetToken  string `json:"reset_token"`
	NewPassword string `json:"new_password"`
}

// ResetPassword 用重置票据设置新密码。
func (h *Auth) ResetPassword(ctx context.Context, c *app.RequestContext) {
	var req resetPasswordRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.ResetPassword(ctx, req.ResetToken, req.NewPassword, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

// Logout 撤销当前会话。
func (h *Auth) Logout(ctx context.Context, c *app.RequestContext) {
	session := auth.CurrentSession(c)
	if session != nil {
		if err := h.svc.Logout(ctx, session.SessionID, auth.ClientInfoOf(c)); err != nil {
			api.Fail(c, err)
			return
		}
		// 丢弃该会话未消费的许可（R-013）：否则一个已经登出的会话仍可能
		// 在许可有效期内消费掉它，用户会看到「已登出却操作成功」。
		h.risk.RevokeSession(session.ID)
	}
	// 无论会话是否已存在，都清掉浏览器 Cookie：否则用户会带着一个
	// 已失效的令牌反复请求，每次都触发一次无谓的 401。
	clearSessionCookie(c, h.secureCookie)
	api.NoContent(c)
}

// Session 返回当前会话与用户信息，供前端启动时恢复登录态。
func (h *Auth) Session(_ context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	session := auth.CurrentSession(c)

	api.OK(c, map[string]any{
		"user":    userViewOf(user),
		"session": sessionViewOf(session, true),
	})
}

// Sessions 返回当前用户的全部有效会话，用于安全中心的「登录记录」。
func (h *Auth) Sessions(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	current := auth.CurrentSession(c)

	list, err := h.svc.Sessions(ctx, user.ID)
	if err != nil {
		api.Fail(c, err)
		return
	}

	views := make([]sessionView, 0, len(list))
	for i := range list {
		isCurrent := current != nil && list[i].ID == current.ID
		views = append(views, *sessionViewOf(&list[i], isCurrent))
	}
	api.OK(c, views)
}

// RevokeSession 撤销指定会话（撤销他人会话返回 404，不泄漏其是否存在）。
func (h *Auth) RevokeSession(ctx context.Context, c *app.RequestContext) {
	// 高风险操作：撤销会话会让被撤销方立即失去访问（f-10-02）。
	if !h.risk.Require(c, risk.ActionSessionRevoke) {
		return
	}

	id, err := namedPathID(c, "id", "会话 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	if err := h.svc.RevokeSession(ctx, user.ID, id, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	// 被撤销的会话若持有未消费的许可，一并丢弃。
	h.risk.RevokeSession(id)
	api.NoContent(c)
}

// setSessionCookie 下发会话 Cookie。
//
// maxAge 与会话过期时间对齐：Cookie 不应比会话活得更久。
func setSessionCookie(c *app.RequestContext, token string, expiresAt time.Time, secure bool) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge <= 0 {
		maxAge = 1
	}
	c.SetCookie(auth.CookieName, token, maxAge, "/", "",
		protocol.CookieSameSiteStrictMode, secure, true)
}

// clearSessionCookie 清除会话 Cookie（MaxAge 为负即让浏览器立即删除）。
func clearSessionCookie(c *app.RequestContext, secure bool) {
	c.SetCookie(auth.CookieName, "", -1, "/", "",
		protocol.CookieSameSiteStrictMode, secure, true)
}

func userViewOf(u *model.User) userView {
	if u == nil {
		return userView{}
	}
	return userView{ID: u.ID, Username: u.Username, Role: u.Role}
}

func sessionViewOf(s *model.Session, current bool) *sessionView {
	if s == nil {
		return nil
	}
	v := &sessionView{
		ID:           s.ID,
		IssuedAt:     s.IssuedAt,
		ExpiresAt:    s.ExpiresAt,
		LastActiveAt: s.LastActiveAt,
		Current:      current,
	}
	if s.ClientIP != nil {
		v.ClientIP = *s.ClientIP
	}
	if s.UserAgent != nil {
		v.UserAgent = *s.UserAgent
	}
	return v
}
