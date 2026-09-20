package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/alert"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
)

// Alert 提供告警中心（F-8-07）。
type Alert struct {
	svc *alert.Service
}

// NewAlert 构造接口。
func NewAlert(svc *alert.Service) *Alert { return &Alert{svc: svc} }

// List 列出未关闭的告警。
func (h *Alert) List(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.List(ctx, alert.ListOptions{
		Status: c.Query("status"),
		Level:  c.Query("level"),
	})
	if err != nil {
		api.Fail(c, err)
		return
	}
	active, danger, err := h.svc.Counts(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{
		"items":  items,
		"active": active,
		"danger": danger,
	})
}

// Ack 确认一条告警。确认**不等于解决**：问题还在，只是不再打扰人。
func (h *Alert) Ack(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "告警 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	if err := h.svc.Ack(ctx, id, authz.ViewerOf(c)); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}

// AckAll 确认当前全部未确认告警。
func (h *Alert) AckAll(ctx context.Context, c *app.RequestContext) {
	n, err := h.svc.AckAll(ctx, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"acknowledged": n})
}
