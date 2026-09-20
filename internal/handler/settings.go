package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/settings"
)

// Settings 提供系统设置接口（F-9-01）。
//
// 权限：**仅管理员**可读写（R-014）。设置变更不得成为绕过权限的通道，
// 因此 `tenant` 连可见性都没有——该要求在路由注册时声明。
type Settings struct {
	svc  *settings.Service
	mail Mailer
}

// Mailer 是发信能力的最小接口（由 internal/mailer 实现）。
type Mailer interface {
	SendTest(ctx context.Context, to string) error
}

// NewSettings 构造设置接口。mail 可为 nil，此时测试发信返回"未配置"。
func NewSettings(svc *settings.Service, mail Mailer) *Settings {
	return &Settings{svc: svc, mail: mail}
}

type testMailRequest struct {
	To string `json:"to"`
}

// TestMail 发送一封测试邮件（F-1-08）。
//
// 用户验证 SMTP 配置**只有这一个手段**：配置写错时的表现是"什么都没发生"，
// 直到哪天有人找回密码才发现——那时已经晚了。因此失败必须给出邮件服务器
// 返回的真实原因，而不是一句"发送失败"。
func (h *Settings) TestMail(ctx context.Context, c *app.RequestContext) {
	var req testMailRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.To == "" {
		api.Fail(c, api.InvalidParameter("请填写接收测试邮件的邮箱"))
		return
	}
	if h.mail == nil {
		api.Fail(c, api.Unavailable("邮件服务不可用"))
		return
	}
	if err := h.mail.SendTest(ctx, req.To); err != nil {
		api.Fail(c, api.Unavailable("测试邮件发送失败："+err.Error()))
		return
	}
	api.OK(c, map[string]any{"sent": true})
}

// List 返回设置项清单（API-036）。
//
// 响应同时包含**元数据与当前值**：前端只渲染，不硬编码第二份（R-004）——
// 两份清单一旦不同步，用户会看到「界面显示的选项与实际校验规则不符」。
func (h *Settings) List(ctx context.Context, c *app.RequestContext) {
	items, groups, err := h.svc.List(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{
		"groups": groups,
		"items":  items,
	})
}

type updateSettingsRequest struct {
	Values map[string]string `json:"values"`
}

// Update 批量更新设置（API-037）。
//
// **部分成功**语义（R-006）：逐项返回结果。原子语义会让用户在一次改五项、
// 其中一项失败时不知道哪些生效了，只能逐项重试去试探。
func (h *Settings) Update(ctx context.Context, c *app.RequestContext) {
	var req updateSettingsRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	results, err := h.svc.Update(ctx, req.Values, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 只要有一项成功就返回 200：整体成败由 results 逐项表达。
	// 全失败时也返回 200 与逐项原因——把它变成 4xx 会让前端既要在
	// catch 里处理失败、又要在 then 里处理「部分失败」，两条路径做同一件事。
	applied := 0
	for _, r := range results {
		if r.Status == settings.StatusApplied {
			applied++
		}
	}

	api.OK(c, map[string]any{
		"results": results,
		"applied": applied,
		"total":   len(results),
	})
}

type rollbackRequest struct {
	Key string `json:"key"`
}

// Rollback 把设置项回滚到最近一次变更前的值（API-038）。
func (h *Settings) Rollback(ctx context.Context, c *app.RequestContext) {
	var req rollbackRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.Key == "" {
		api.Fail(c, api.InvalidParameter("必须指定要回滚的设置项"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	item, err := h.svc.Rollback(ctx, req.Key, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, item)
}
