package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/search"
)

// Search 提供跨资源检索（F-9-08）。
type Search struct {
	svc *search.Service
}

// NewSearch 构造接口。
func NewSearch(svc *search.Service) *Search { return &Search{svc: svc} }

// Query 按关键字检索虚拟机 / 节点 / 模板。
//
// 结果**按角色收敛**：租户搜不到节点。搜索框是最容易意外泄漏信息的地方——
// 它看起来只是"帮你找东西"，实际能枚举出整个平台的资源名。
func (h *Search) Query(ctx context.Context, c *app.RequestContext) {
	q := c.Query("q")
	matches, err := h.svc.Search(ctx, q, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"matches": matches})
}
