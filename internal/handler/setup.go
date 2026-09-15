package handler

import (
	"context"
	"log"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
)

// Setup 提供首次初始化接口。
//
// 这两个接口在系统未初始化时**必须公开**：此刻系统中还没有任何账号，
// 不可能要求认证——这是自举问题。安全性由「一次性令牌只能从服务端日志
// 获取」保证，而非由认证保证（见 docs/06-decisions/0008-first-admin-bootstrap.md）。
type Setup struct {
	bootstrap    *auth.Bootstrap
	auth         *auth.Service
	secureCookie bool
}

// NewSetup 构造初始化接口。
func NewSetup(bootstrap *auth.Bootstrap, authSvc *auth.Service, secureCookie bool) *Setup {
	return &Setup{bootstrap: bootstrap, auth: authSvc, secureCookie: secureCookie}
}

type setupStatusResponse struct {
	Initialized bool `json:"initialized"`
}

// Status 返回系统是否已初始化，供前端决定跳初始化页还是登录页。
func (h *Setup) Status(ctx context.Context, c *app.RequestContext) {
	initialized, err := h.bootstrap.Initialized(ctx)
	if err != nil {
		log.Printf("[setup] 查询初始化状态失败: %v", err)
		api.Fail(c, api.Unavailable("无法获取初始化状态"))
		return
	}
	api.OK(c, setupStatusResponse{Initialized: initialized})
}

type createAdminRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type createAdminResponse struct {
	User userView `json:"user"`
	// AutoLogin 表示是否已自动建立会话。为 false 时前端应引导用户去登录页
	// ——初始化本身已成功，不能让它表现成失败。
	AutoLogin bool `json:"auto_login"`
}

// CreateAdmin 使用一次性令牌创建首个管理员。
//
// 成功后直接建立会话：用户刚用令牌证明了身份，再要求登录一次是纯粹的摩擦。
func (h *Setup) CreateAdmin(ctx context.Context, c *app.RequestContext) {
	var req createAdminRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	info := auth.ClientInfoOf(c)
	user, err := h.bootstrap.CreateAdmin(ctx, req.Token, req.Username, req.Password, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	resp := createAdminResponse{User: userViewOf(user)}
	if res, err := h.auth.Login(ctx, req.Username, req.Password, info); err == nil {
		setSessionCookie(c, res.Token, res.ExpiresAt, h.secureCookie)
		resp.AutoLogin = true
	} else {
		// 账号已经创建成功，自动登录失败不能让调用方以为初始化失败。
		log.Printf("[setup] 初始化后自动登录失败: %v", err)
	}

	api.Created(c, resp)
}
