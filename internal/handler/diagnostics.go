package handler

import (
	"context"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/diagnostics"
)

// Diagnostics 提供诊断导出（F-9-03）。归管理员。
//
// 归管理员的理由：这个包里是**整个系统的内部状态**（全部设置、全部审计
// 日志）。它对运维排障有用，而对租户没有意义——租户关心的是自己那台机器，
// 而审计日志里含别人的操作记录。
//
// 它**不需要二次验证**，这一点是刻意的：排障包最常见的用法是"出问题的时候
// 赶紧导出来发给支持"，而在那一刻再拦一道验证会让最需要它的时候最不好用。
// 之所以敢这样，是因为**脱敏发生在打包时**——包里不含任何密钥明文，
// 它本身就是可以外发的（见 diagnostics 包注释第 1 条）。
type Diagnostics struct {
	svc *diagnostics.Service
}

// NewDiagnostics 构造接口。
func NewDiagnostics(svc *diagnostics.Service) *Diagnostics {
	return &Diagnostics{svc: svc}
}

// Categories 列出可导出的分类（API-270）。
func (h *Diagnostics) Categories(ctx context.Context, c *app.RequestContext) {
	api.OK(c, map[string]any{"items": diagnostics.Categories()})
}

// Export 生成并下载诊断包（API-271）。
func (h *Diagnostics) Export(ctx context.Context, c *app.RequestContext) {
	// 分类用逗号分隔；为空表示全选。
	var keys []string
	if raw := strings.TrimSpace(c.Query("categories")); raw != "" {
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	bundle, err := h.svc.Export(ctx, keys, diagnostics.Viewer{
		UserID: user.ID, IsAdmin: user.IsAdmin(), Username: user.Username,
	}, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 响应头**必须设对**：Content-Disposition 里的文件名带了生成时刻，
	// 而用户会下载很多份——名字一样的话，浏览器会加 (1)(2)，几轮之后就
	// 分不清哪份是哪次导出的。
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", `attachment; filename="`+bundle.Filename+`"`)
	if len(bundle.Truncated) > 0 {
		// 截断信息同时放进响应头：界面要据此提示，而它没法从 zip 里读。
		c.Header("X-KC-Truncated", strings.Join(bundle.Truncated, " | "))
	}
	c.Response.SetBody(bundle.Data)
}
