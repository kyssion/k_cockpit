package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/apikey"
	"k_cockpit/internal/auth"
)

// APIKey 提供 API 凭证接口（F-1-10）。
//
// 这些接口**只走会话认证**（`h.svc` 之外还要求 `CurrentSession` 非空）：
// 不允许用一个 API Key 去重新生成或撤销 API Key。理由很直接——这样会形成
// 一个自我延续的凭据链：一个泄漏的 Key 可以立刻轮换出一个新的，
// 而原持有者撤销它时，攻击者手上那个仍然有效。
type APIKey struct {
	svc *apikey.Service
}

// NewAPIKey 构造接口。
func NewAPIKey(svc *apikey.Service) *APIKey {
	return &APIKey{svc: svc}
}

// requireSession 要求本次请求是通过会话认证的。
func requireSession(c *app.RequestContext) bool {
	if auth.CurrentSession(c) == nil {
		api.Fail(c, api.ValidationFailed(
			"该操作需要用登录会话完成，不能使用 API 凭证——"+
				"否则一个泄漏的凭证可以自行轮换出新的，原持有者撤销它也拦不住"))
		return false
	}
	return true
}

// Get 返回凭证信息（API-170）。
func (h *APIKey) Get(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	view, err := h.svc.Get(ctx, user.ID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type createAPIKeyRequest struct {
	AllowedIPs    []string `json:"allowed_ips"`
	ExpiresInDays int      `json:"expires_in_days"`
}

// Create 生成或轮换凭证（API-171）。
//
// **响应是唯一一次携带明文的地方**。库里只存哈希，服务端之后再也拿不到
// 它——因此界面必须明确告知「现在就复制」。
func (h *APIKey) Create(ctx context.Context, c *app.RequestContext) {
	if !requireSession(c) {
		return
	}
	var req createAPIKeyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	created, err := h.svc.Create(ctx, user.ID, apikey.CreateRequest{
		AllowedIPs: req.AllowedIPs, ExpiresInDays: req.ExpiresInDays,
	}, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, created)
}

// Revoke 撤销凭证（API-172）。
func (h *APIKey) Revoke(ctx context.Context, c *app.RequestContext) {
	if !requireSession(c) {
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Revoke(ctx, user.ID, user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"revoked": true})
}

type issueTokenRequest struct {
	// Purpose 说明这个令牌要用来做什么。
	Purpose string `json:"purpose"`
}

// IssueActionToken 签发一次性动作令牌（API-173）。
//
// 界面用它来给浏览器一个能直接下载的 URL：`<a href>` 带不上自定义请求头，
// 因此用 Cookie 认证的接口没法直接下给用户。做法是先用会话换一个短期令牌，
// 再把令牌放进 URL。
func (h *APIKey) IssueActionToken(ctx context.Context, c *app.RequestContext) {
	if !requireSession(c) {
		return
	}
	var req issueTokenRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	purpose := req.Purpose
	if purpose == "" {
		purpose = "download"
	}

	user := auth.CurrentUser(c)
	plain, expiresAt, err := h.svc.IssueActionToken(ctx, user.ID, purpose)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{
		"token":      plain,
		"expires_at": expiresAt.Format("2006-01-02T15:04:05Z07:00"),
		// 明确告知它只能用一次：界面据此决定是否提示「刷新后需重新获取」。
		"single_use": true,
	})
}
