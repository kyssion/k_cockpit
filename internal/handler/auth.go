package handler

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// Auth 提供认证相关接口。
type Auth struct {
	svc          *auth.Service
	secureCookie bool
}

// NewAuth 构造认证接口。
//
// secureCookie 在 HTTPS 部署时必须为 true；本地 HTTP 开发必须为 false，
// 否则浏览器不会回传带 Secure 标记的 Cookie，表现为「登录成功但仍未认证」。
func NewAuth(svc *auth.Service, secureCookie bool) *Auth {
	return &Auth{svc: svc, secureCookie: secureCookie}
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
	User      userView  `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

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

	setSessionCookie(c, res.Token, res.ExpiresAt, h.secureCookie)
	api.OK(c, loginResponse{User: userViewOf(res.User), ExpiresAt: res.ExpiresAt})
}

// Logout 撤销当前会话。
func (h *Auth) Logout(ctx context.Context, c *app.RequestContext) {
	session := auth.CurrentSession(c)
	if session != nil {
		if err := h.svc.Logout(ctx, session.SessionID, auth.ClientInfoOf(c)); err != nil {
			api.Fail(c, err)
			return
		}
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
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		api.Fail(c, api.InvalidParameter("会话 ID 不合法"))
		return
	}

	user := auth.CurrentUser(c)
	if err := h.svc.RevokeSession(ctx, user.ID, id, auth.ClientInfoOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
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
