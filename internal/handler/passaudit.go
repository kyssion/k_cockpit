package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/passaudit"
)

// PassAudit 提供口令检查接口（F-10-06）。
type PassAudit struct {
	svc *passaudit.Service
}

// NewPassAudit 构造接口。
func NewPassAudit(svc *passaudit.Service) *PassAudit { return &PassAudit{svc: svc} }

// RunNow 立即检查一次。
func (h *PassAudit) RunNow(ctx context.Context, c *app.RequestContext) {
	res, err := h.svc.RunNow(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	if res.Unavailable != "" {
		// 没检查成也要给出结果，而不是当错误：区别在于"要不要重试"。
		api.OK(c, res)
		return
	}
	api.OK(c, res)
}

// Status 返回当前处于命中状态的账号数。
func (h *PassAudit) Status(ctx context.Context, c *app.RequestContext) {
	n, err := h.svc.Status(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"hits": n})
}
