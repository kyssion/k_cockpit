package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authkey"
	"k_cockpit/internal/risk"
)

// AuthKey 提供会话签名密钥的轮换入口（F-1-09）。
type AuthKey struct {
	svc  *authkey.Service
	risk *risk.Guard
}

// NewAuthKey 构造接口。
func NewAuthKey(svc *authkey.Service, g *risk.Guard) *AuthKey { return &AuthKey{svc: svc, risk: g} }

// riskGuard 是 risk.Guard.Require 的简写，避免本文件里重复那段"只有高风险
// 动作才需要"的说明。
func (h *AuthKey) riskGuard(c *app.RequestContext, a risk.Action) bool {
	if h.risk == nil {
		return true
	}
	return h.risk.Require(c, a)
}

// Status 返回当前密钥的状态（不含密钥本身）。
func (h *AuthKey) Status(ctx context.Context, c *app.RequestContext) {
	st, err := h.svc.Current(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, st)
}

// Rotate 轮换签名密钥。
//
// 受二次验证保护：它等价于一次**全员登出**，而误点一次会让所有人（包括正在
// 操作的这位管理员自己）被踢出去。
func (h *AuthKey) Rotate(ctx context.Context, c *app.RequestContext) {
	if !h.riskGuard(c, risk.ActionAuthKeyRotate) {
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	st, err := h.svc.Rotate(ctx, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, st)
}
