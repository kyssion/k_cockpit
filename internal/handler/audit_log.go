package handler

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auditlog"
	"k_cockpit/internal/authz"
)

// AuditLog 提供审计流水的查询（F-1-12）。
type AuditLog struct {
	svc *auditlog.Service
}

// NewAuditLog 构造接口。
func NewAuditLog(svc *auditlog.Service) *AuditLog {
	return &AuditLog{svc: svc}
}

// List 查询审计流水（API-200）。
//
// **权限隔离在服务层做**，这里只负责把查询参数搬过去。放在服务层的理由：
// 之后无论谁加新的查询入口，都绕不过那一条判断——而把它写在这里，等于
// 每加一个入口就要记得重写一遍。
func (h *AuditLog) List(ctx context.Context, c *app.RequestContext) {
	f := auditlog.Filter{
		Page:         queryInt(c, "page"),
		PageSize:     queryInt(c, "page_size"),
		OperatorID:   int64(queryInt(c, "operator_id")),
		ResourceType: c.Query("resource_type"),
		Action:       c.Query("action"),
		ResourceID:   int64(queryInt(c, "resource_id")),
		NodeID:       int64(queryInt(c, "node_id")),
		Keyword:      c.Query("keyword"),
		// 来源（G-38）：web / api / system / emergency，空串表示不限。
		Source: c.Query("source"),
	}
	if v := c.Query("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.From = t
		}
	}
	if v := c.Query("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.To = t
		}
	}
	// success 用三态：不传 = 不限，true/false = 明确筛选。
	if v := c.Query("success"); v == "true" || v == "false" {
		b := v == "true"
		f.Success = &b
	}

	page, err := h.svc.List(ctx, f, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, page)
}

// Get 读取一条记录（API-201）。
func (h *AuditLog) Get(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "记录 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	view, err := h.svc.Get(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Facets 返回可用的筛选项（API-202）。
//
// 动作名由服务端给出而不是让界面写死：动作会随功能增加而变化，写死的列表
// 迟早与后端对不上，而「筛选里选不到某个刚加的动作」很难被发现。
func (h *AuditLog) Facets(ctx context.Context, c *app.RequestContext) {
	facets, err := h.svc.Facets(ctx, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, facets)
}
