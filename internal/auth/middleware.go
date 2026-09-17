package auth

import (
	"context"
	"strings"

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

// APIKeyAuth 是 API 凭证的校验入口。
//
// 声明成接口而不是直接依赖 apikey 包：auth 是几乎所有包都会引入的底层包，
// 让它反过来依赖 apikey 会把依赖图绕成一个环（apikey → audit → … → auth）。
// 接口在这里定义、由 apikey 实现，方向就是单向的。
type APIKeyAuth interface {
	// Authenticate 校验明文凭证；返回 nil 表示不通过。
	Authenticate(ctx context.Context, plain, fromIP string) (*APIKeyPrincipal, error)
}

// APIKeyPrincipal 是凭证校验通过后的身份。
type APIKeyPrincipal struct {
	UserID int64
	KeyID  int64
	Prefix string
}

// Middleware 提供认证中间件。
type Middleware struct {
	svc *Service
	// apiKey 为 nil 时**不支持** API 凭证认证（如测试环境）。
	apiKey APIKeyAuth
}

// NewMiddleware 构造认证中间件。
func NewMiddleware(svc *Service) *Middleware {
	return &Middleware{svc: svc}
}

// WithAPIKey 挂上 API 凭证认证。
//
// 用链式设置而不是塞进构造函数：现有的调用点（尤其是大量测试）不需要为了
// 一个可选能力改动签名。
func (m *Middleware) WithAPIKey(a APIKeyAuth) *Middleware {
	m.apiKey = a
	return m
}

// Require 返回认证中间件：校验令牌与会话，通过后把当前用户注入上下文。
//
// activity 决定本次请求是否计入会话续期。**轮询与实时通道心跳必须传
// Passive**——否则用户开着页面不动也会让会话无限续期，空闲超时形同虚设
// （f-1-01 R-008）。
func (m *Middleware) Require(activity Activity) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		// API 凭证优先看 Authorization 头。
		//
		// 顺序上它排在 Cookie 之前有一个具体原因：**脚本不会带 Cookie**，
		// 而浏览器会同时带两者（当用户在页面上操作、同时又有自动化在跑时）。
		// 让显式给出的 Authorization 优先，符合「调用方明确说了用什么身份」
		// 这一直觉。
		if m.apiKey != nil {
			if plain, ok := bearerToken(c); ok {
				if m.authenticateByKey(ctx, c, plain) {
					return
				}
				// 给了 Authorization 但校验不通过时**直接拒绝**，不再回落到
				// Cookie。回落会让一个已撤销的 Key 在浏览器会话仍然有效时
				// 继续「工作」，而用户以为自己已经把它撤销了。
				api.Fail(c, errUnauthenticated)
				c.Abort()
				return
			}
		}

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

// authenticateByKey 用 API 凭证认证，成功返回 true。
//
// **不设置会话**：API 凭证没有会话可言，而造一个假的会话对象会让
// 「会话管理」（登出、列出会话、撤销会话）这些接口以为自己在操作一个
// 真实会话。设成 nil 之后，那些接口的既有判空逻辑会正确地拒绝它。
func (m *Middleware) authenticateByKey(
	ctx context.Context, c *app.RequestContext, plain string,
) bool {
	p, err := m.apiKey.Authenticate(ctx, plain, ClientInfoOf(c).IP)
	if err != nil || p == nil {
		return false
	}

	var user model.User
	if err := m.svc.db.WithContext(ctx).Where("id = ?", p.UserID).First(&user).Error; err != nil {
		// 用户被删除后凭证也随之失效。不区分「查不到」与「查出错」：
		// 两者对外都是同一句「凭证无效」。
		return false
	}
	// 封禁的用户即使持有有效凭证也不能用——否则「封禁」会被一个
	// 早已生成的 Key 绕过。
	if user.Status != model.UserStatusActive {
		return false
	}

	c.Set(ctxKeyUser, &user)
	return true
}

// bearerToken 从 Authorization 头里取出 Bearer 令牌。
func bearerToken(c *app.RequestContext) (string, bool) {
	raw := string(c.GetHeader("Authorization"))
	const prefix = "Bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(raw[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
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
