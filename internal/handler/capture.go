package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/capture"
)

// Capture 提供抓包接口（F-4-12）。
type Capture struct {
	svc *capture.Service
}

// NewCapture 构造接口。
func NewCapture(svc *capture.Service) *Capture { return &Capture{svc: svc} }

// List 返回节点上的抓包记录（API-260）。
func (h *Capture) List(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.List(ctx, nodeID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type startCaptureRequest struct {
	VMID        int64  `json:"vm_id"`
	Interface   string `json:"interface"`
	Filter      string `json:"filter"`
	DurationSec int    `json:"duration_sec"`
}

// Start 发起抓包（API-261）。
func (h *Capture) Start(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req startCaptureRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, t, err := h.svc.Start(ctx, capture.Request{
		NodeID: nodeID, VMID: req.VMID, Interface: req.Interface,
		Filter: req.Filter, DurationSec: req.DurationSec,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"capture": view, "task": t})
}

// Delete 删除抓包（API-262）。
//
// **文件与控制面记录一起删**：只删记录会让一份含明文流量的文件永远留在
// 宿主机上，而界面上已经看不到它。
func (h *Capture) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "抓包 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Delete(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// Download 取回一份抓包文件（G-38）。
//
// 内容经控制面转发（归属校验与审计都在服务层），响应头声明 pcap 类型与
// 文件名；不缓存——文件虽然不可变，但它含完整流量内容，减少一份浏览器
// 磁盘缓存里的副本是更稳妥的默认。
func (h *Capture) Download(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "抓包 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	name, data, mime, err := h.svc.File(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", contentDisposition(name))
	c.Header("Cache-Control", "no-store")
	c.SetContentType(mime)
	c.Response.SetBody(data)
}
