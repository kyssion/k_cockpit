package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/publicip"
)

// PublicIP 提供公网 IP 接口（F-4-06）。
//
// 权限：地址池是**节点级资源**，因此整体归 admin（与节点、存储池同一档）。
// 单个租户能绑哪些地址属于配额范畴（f-1-08 把「公网 IP」列为配额维度），
// 那是后续的事——先让地址能录进来、能绑上去。
type PublicIP struct {
	svc *publicip.Service
}

// NewPublicIP 构造公网 IP 接口。
func NewPublicIP(svc *publicip.Service) *PublicIP {
	return &PublicIP{svc: svc}
}

// List 返回节点上的地址池（API-100）。
func (h *PublicIP) List(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.List(ctx, int64(queryInt(c, "node_id")))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type createPublicIPRequest struct {
	NodeID   int64    `json:"node_id"`
	IP       string   `json:"ip"`
	Gateway  string   `json:"gateway"`
	EgressIf string   `json:"egress_if"`
	Modes    []string `json:"supported_modes"`
	Remark   string   `json:"remark"`
}

// Create 录入地址（API-100）。支持单个地址或 CIDR 批量。
func (h *PublicIP) Create(ctx context.Context, c *app.RequestContext) {
	var req createPublicIPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	items, err := h.svc.Create(ctx, publicip.CreateRequest{
		NodeID: req.NodeID, IP: req.IP, Gateway: req.Gateway,
		EgressIf: req.EgressIf, SupportedModes: req.Modes, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Delete 从池中移除地址（API-101）。已绑定时拒绝。
func (h *PublicIP) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "地址 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Delete(ctx, id, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

type bindPublicIPRequest struct {
	VMID int64  `json:"vm_id"`
	Mode string `json:"mode"`
}

// Bind 绑定地址到虚拟机（API-102）。
func (h *PublicIP) Bind(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "地址 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req bindPublicIPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: id, VMID: req.VMID, Mode: req.Mode,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

type migratePublicIPRequest struct {
	ToVMID int64  `json:"to_vm_id"`
	Mode   string `json:"mode"`
}

// Migrate 浮动迁移（API-103）。
//
// **不需要二次验证**：迁移不会造成不可逆的结果——地址还在池里，随时可以
// 迁回来。它影响的是连通性，而中断是**立刻可见**的：用户马上就知道发生了
// 什么，不需要一道验证来提醒他。
func (h *PublicIP) Migrate(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "地址 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req migratePublicIPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Migrate(ctx, publicip.MigrateRequest{
		PublicIPID: id, ToVMID: req.ToVMID, Mode: req.Mode,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// Unbind 解绑（API-104）。
func (h *PublicIP) Unbind(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "地址 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Unbind(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// Preview 规则预览（API-105）。
//
// **只读探测**：不产生任何改动、不入队。它必须来自节点而不是控制面拼出来
// ——规则的最终形态取决于宿主机上已有的链、路由表与网卡配置。
func (h *PublicIP) Preview(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "地址 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	preview, err := h.svc.Preview(ctx, id,
		int64(queryInt(c, "vm_id")), c.Query("mode"), authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, preview)
}

// BatchUnbind 批量解绑（API-300）。
//
// **逐条调用单条方法**，复用全部校验——在本处重写一遍的话，两处迟早会分叉，
// 而分叉的表现是「单个能解绑、批量里解不了」。
func (h *PublicIP) BatchUnbind(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	res, err := h.svc.BatchUnbind(ctx, req.IDs, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, res)
}

// BatchBind 批量绑定到同一台虚拟机（API-301）。
func (h *PublicIP) BatchBind(ctx context.Context, c *app.RequestContext) {
	var req struct {
		IDs  []int64 `json:"ids"`
		VMID int64   `json:"vm_id"`
		Mode string  `json:"mode"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	res, err := h.svc.BatchBind(ctx, req.IDs, req.VMID, req.Mode,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, res)
}
