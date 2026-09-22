package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/invite"
)

// Invite 提供邀请注册接口（F-1-10）。
type Invite struct {
	svc *invite.Service
}

// NewInvite 构造接口。
func NewInvite(svc *invite.Service) *Invite { return &Invite{svc: svc} }

type createInviteRequest struct {
	Email        string `json:"email"`
	Role         string `json:"role"`
	QuotaEnabled bool   `json:"quota_enabled"`
	QuotaBytes   int64  `json:"quota_bytes"`
	Remark       string `json:"remark"`
	TTLHours     int    `json:"ttl_hours"`
}

// Create 创建一条邀请（管理员）。
func (h *Invite) Create(ctx context.Context, c *app.RequestContext) {
	var req createInviteRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Create(ctx, invite.CreateRequest{
		Email: req.Email, Role: req.Role,
		QuotaEnabled: req.QuotaEnabled, QuotaBytes: req.QuotaBytes,
		Remark: req.Remark, TTLHours: req.TTLHours,
	}, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// List 列出邀请（管理员）。
func (h *Invite) List(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.List(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Revoke 撤销邀请（管理员）。
func (h *Invite) Revoke(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "邀请 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Revoke(ctx, id, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Resend 重发邀请（管理员）：换新令牌并延长有效期。
func (h *Invite) Resend(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "邀请 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Resend(ctx, id, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Preview 公开预览：拿到链接的人先看这是给谁、什么角色。
func (h *Invite) Preview(ctx context.Context, c *app.RequestContext) {
	view, err := h.svc.Preview(ctx, c.Query("token"))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type acceptInviteRequest struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Email 留空表示用邀请时的邮箱。
	Email string `json:"email"`
}

// Accept 接受邀请（公开）。
//
// 它**不建会话**：注册完需要自己登录一次。自动登录会让"这个账号到底是谁在用"
// 变得含糊，而登录本身也是一次确认——他知道自己的密码。
func (h *Invite) Accept(ctx context.Context, c *app.RequestContext) {
	var req acceptInviteRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if err := h.svc.Accept(ctx, invite.AcceptRequest{
		Token: req.Token, Username: req.Username, Password: req.Password, Email: req.Email,
	}); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}
