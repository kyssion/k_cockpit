package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/template"
)

type maintainRequest struct {
	ChildID     int64 `json:"child_id"`
	Acknowledge bool  `json:"acknowledge"`
}

// RebaseTemplate 把模板的 backing 切换到上级（缩短派生链）。
func (h *Template) RebaseTemplate(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req maintainRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Maintain(ctx, id, template.MaintainRequest{
		Action: "rebase", Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// FlattenTemplate 在线拉平到上级（不中断使用这块盘的虚拟机）。
func (h *Template) FlattenTemplate(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req maintainRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Maintain(ctx, id, template.MaintainRequest{
		Action: "flatten", Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// PromoteChildTemplate 把某个子模板提升一级。
func (h *Template) PromoteChildTemplate(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req maintainRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Maintain(ctx, id, template.MaintainRequest{
		Action: "promote_child", ChildID: req.ChildID, Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// PromoteDeleteTemplate 删除这一代，并把下游模板改挂到上级。
//
// 这是"删掉中间一代"唯一安全的入口：直接删除会让下游全部失效，而节点上的
// 表现是"虚拟机还在跑，读某个块时才报错"。
func (h *Template) PromoteDeleteTemplate(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req maintainRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Maintain(ctx, id, template.MaintainRequest{
		Action: "promote_delete", Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
