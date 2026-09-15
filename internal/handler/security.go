package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
	"k_cockpit/internal/risk"
)

// Security 提供高风险操作的二次验证与绑定接口（F-10-01 / F-10-02）。
type Security struct {
	guard *risk.Guard
	auth  *auth.Service
}

// NewSecurity 构造安全接口。
func NewSecurity(guard *risk.Guard, svc *auth.Service) *Security {
	return &Security{guard: guard, auth: svc}
}

// Policy 返回高风险操作清单与判定口径（API-034）。
//
// **只读**，且返回的是「口径与清单」，不是「当前用户是否需要验证」——
// 后者由具体操作请求的响应（428）决定。让前端据此提前提示，但不能据此
// 决定是否验证（R-003）。
func (h *Security) Policy(_ context.Context, c *app.RequestContext) {
	api.OK(c, map[string]any{
		"criteria":          "会造成不可逆结果，或影响可达性",
		"actions":           risk.Policy(),
		"grant_header":      risk.GrantHeader,
		"grant_ttl_seconds": int(h.guard.Expire().Seconds()),
	})
}

type verifyRequest struct {
	ChallengeID string `json:"challenge_id"`
	Method      string `json:"method"`
	Code        string `json:"code"`
}

// Verify 提交验证码，换取一次性许可（API-035）。
//
// 返回的许可由前端在**重放原请求**时通过请求头携带。许可不在响应中回显
// 验证码，服务端也不记录验证码明文。
func (h *Security) Verify(ctx context.Context, c *app.RequestContext) {
	var req verifyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	session := auth.CurrentSession(c)
	if user == nil || session == nil {
		api.Fail(c, api.Unauthenticated("请先登录"))
		return
	}

	grant, err := h.guard.Verify(ctx, c, user, session, risk.VerifyRequest{
		ChallengeID: req.ChallengeID,
		Method:      req.Method,
		Code:        risk.NormalizeCode(req.Code),
	})
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{
		"grant":      grant.Token,
		"method":     grant.Method,
		"expires_at": grant.ExpiresAt,
	})
}

// SetupStatus 返回当前账号的二次验证绑定进度。
func (h *Security) SetupStatus(ctx context.Context, c *app.RequestContext) {
	user, err := h.reloadUser(ctx, c)
	if err != nil {
		api.Fail(c, err)
		return
	}

	session := auth.CurrentSession(c)
	info := risk.SetupState(user)
	api.OK(c, map[string]any{
		"totp_enabled":        info.TOTPEnabled,
		"pending_setup":       info.PendingSetup,
		"recovery_code_count": info.RecoveryCodes,
		"has_recovery_codes":  info.RecoveryCodesOK,
		"methods":             risk.AvailableMethods(user),
		"session_id":          session.ID,
	})
}

// BeginTOTP 开始绑定验证器，返回供二维码使用的 otpauth URI。
func (h *Security) BeginTOTP(ctx context.Context, c *app.RequestContext) {
	user, err := h.reloadUser(ctx, c)
	if err != nil {
		api.Fail(c, err)
		return
	}

	uri, err := h.guard.BeginTOTPSetup(ctx, user)
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 不返回密钥本身，只返回 URI：URI 里已经含密钥，多给一份明文只会
	// 增加它出现在日志或前端状态里的机会。
	api.OK(c, map[string]any{"otpauth_uri": uri})
}

type confirmTOTPRequest struct {
	Code string `json:"code"`
}

// ConfirmTOTP 提交一次动态码以启用绑定，并返回恢复码（**只返回一次**）。
func (h *Security) ConfirmTOTP(ctx context.Context, c *app.RequestContext) {
	var req confirmTOTPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user, err := h.reloadUser(ctx, c)
	if err != nil {
		api.Fail(c, err)
		return
	}

	codes, err := h.guard.ConfirmTOTPSetup(ctx, user, risk.NormalizeCode(req.Code))
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{
		"recovery_codes": codes,
		"notice":         "恢复码只显示这一次，请立即保存到安全的地方。每个恢复码只能用一次。",
	})
}

// reloadUser 重新读取用户记录。
//
// 认证中间件注入的用户对象是**本次请求开始时的快照**，而绑定流程会改动它
// （例如刚生成的 TOTP 密钥）。用旧快照会导致「刚生成的密钥在这里读不到」，
// 表现为「请先开始绑定流程」——而用户显然刚点过。
func (h *Security) reloadUser(ctx context.Context, c *app.RequestContext) (*model.User, error) {
	current := auth.CurrentUser(c)
	if current == nil {
		return nil, api.Unauthenticated("请先登录")
	}
	return h.auth.UserByID(ctx, current.ID)
}
