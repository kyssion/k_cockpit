package auth

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// CookieName 是承载访问令牌的 Cookie 名。
//
// 令牌经 HttpOnly + Secure + SameSite=Strict Cookie 下发，而不是 localStorage
// （f-1-01 Q-008）：XSS 无法读取 HttpOnly Cookie，从浏览器层面根除「脚本窃取
// 令牌」这一最主要的泄漏途径。本项目同源部署，SameSite=Strict 不产生额外成本。
const CookieName = "kc_session"

// 当前用户与会话在 RequestContext 中的键。
const (
	ctxKeyUser    = "current_user"
	ctxKeySession = "current_session"
)

// Middleware 提供认证中间件。
type Middleware struct {
	svc *Service
}

// NewMiddleware 构造认证中间件。
func NewMiddleware(svc *Service) *Middleware {
	return &Middleware{svc: svc}
}

// Require 返回认证中间件：校验令牌与会话，通过后把当前用户注入上下文。
//
// activity 决定本次请求是否计入会话续期。**轮询与实时通道心跳必须传
// Passive**——否则用户开着页面不动也会让会话无限续期，空闲超时形同虚设
// （f-1-01 R-008）。
func (m *Middleware) Require(activity Activity) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		token := string(c.Cookie(CookieName))
		if token == "" {
			api.Fail(c, errUnauthenticated)
			c.Abort()
			return
		}

		user, session, err := m.svc.Authenticate(ctx, token, ClientInfoOf(c), activity)
		if err != nil {
			api.Fail(c, err)
			c.Abort()
			return
		}

		c.Set(ctxKeyUser, user)
		c.Set(ctxKeySession, session)
		c.Next(ctx)
	}
}

// Optional 返回「有则解析、无则放行」的中间件。
//
// 只用于登录页之类需要区分「已登录 / 未登录」但都不算错误的场景；
// 业务接口一律用 Require。
func (m *Middleware) Optional() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		token := string(c.Cookie(CookieName))
		if token == "" {
			c.Next(ctx)
			return
		}

		user, session, err := m.svc.Authenticate(ctx, token, ClientInfoOf(c), Passive)
		if err == nil {
			c.Set(ctxKeyUser, user)
			c.Set(ctxKeySession, session)
		}
		c.Next(ctx)
	}
}

// CurrentUser 返回当前登录用户；未认证时为 nil。
func CurrentUser(c *app.RequestContext) *model.User {
	if v, ok := c.Get(ctxKeyUser); ok {
		if u, ok := v.(*model.User); ok {
			return u
		}
	}
	return nil
}

// CurrentSession 返回当前会话；未认证时为 nil。
func CurrentSession(c *app.RequestContext) *model.Session {
	if v, ok := c.Get(ctxKeySession); ok {
		if s, ok := v.(*model.Session); ok {
			return s
		}
	}
	return nil
}

// ClientInfoOf 从请求中提取客户端信息，用于会话指纹与审计。
func ClientInfoOf(c *app.RequestContext) ClientInfo {
	return ClientInfo{
		IP:        c.ClientIP(),
		UserAgent: string(c.GetHeader("User-Agent")),
	}
}
