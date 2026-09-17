package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/vmtag"
)

// VMTag 提供虚拟机标签接口（F-2-16）。
type VMTag struct {
	svc *vmtag.Service
}

// NewVMTag 构造接口。
func NewVMTag(svc *vmtag.Service) *VMTag { return &VMTag{svc: svc} }

// List 返回一台虚拟机的标签（API-220）。
func (h *VMTag) List(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	tags, err := h.svc.Tags(ctx, vmID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"tags": tags})
}

type setTagsRequest struct {
	// Tags 是**整体替换**而不是增量。界面改完直接保存，而 add/remove 逐个
	// 操作会让「加了又删、删了又加」的中间态被如实写进审计流水。
	Tags []string `json:"tags"`
}

// Set 整体替换一台虚拟机的标签（API-221）。
func (h *VMTag) Set(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req setTagsRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	tags, err := h.svc.SetTags(ctx, vmID, req.Tags, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"tags": tags})
}

// All 返回该调用者可见范围内的全部标签及使用次数（API-222）。
//
// 返回计数而不是纯列表：界面上「生产 (12)」比「生产」有用——它能让人判断
// 这个标签是不是一个只有一台机器的孤儿标签。
func (h *VMTag) All(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.AllTags(ctx, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// VMsByTag 返回带某个标签的虚拟机 id（API-223）。
//
// 不由界面自己筛：列表是分页的，前端只拿得到当前页，而「哪些机器带这个
// 标签」必须看全量。
func (h *VMTag) VMsByTag(ctx context.Context, c *app.RequestContext) {
	ids, err := h.svc.VMsByTag(ctx, c.Query("tag"), authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"vm_ids": ids})
}
