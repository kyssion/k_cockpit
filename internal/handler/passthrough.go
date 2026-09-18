package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/passthrough"
)

// Passthrough 提供 PCIe 直通接口。归管理员。
//
// 归管理员的理由很直接：绑定设备会**改变宿主机上设备的归属**——把一块卡
// 从宿主驱动抢过来，宿主就看不到它了。如果那块卡正在被宿主使用（例如它是
// 唯一的网卡），绑定会让宿主机**当场失去网络**，而面板正是通过网络管理的。
// 那不是租户该有的能力。
type Passthrough struct {
	svc *passthrough.Service
}

// NewPassthrough 构造接口。
func NewPassthrough(svc *passthrough.Service) *Passthrough {
	return &Passthrough{svc: svc}
}

// Overview 返回设备清单与 IOMMU 状态（API-320）。
func (h *Passthrough) Overview(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	devices, iommu, err := h.svc.Overview(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"devices": devices, "iommu": iommu})
}

// Bind 绑定设备到 vfio-pci（API-321）。
func (h *Passthrough) Bind(ctx context.Context, c *app.RequestContext) {
	h.bindOrUnbind(ctx, c, true)
}

// Unbind 解绑设备（API-322）。
func (h *Passthrough) Unbind(ctx context.Context, c *app.RequestContext) {
	h.bindOrUnbind(ctx, c, false)
}

func (h *Passthrough) bindOrUnbind(ctx context.Context, c *app.RequestContext, bind bool) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req struct {
		Address string `json:"pci_address"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	var err error
	if bind {
		err = h.svc.Bind(ctx, nodeID, req.Address, authz.ViewerOf(c), user.Username, info.IP)
	} else {
		err = h.svc.Unbind(ctx, nodeID, req.Address, authz.ViewerOf(c), user.Username, info.IP)
	}
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}

// ListVM 返回虚拟机挂载的直通设备（API-323）。
func (h *Passthrough) ListVM(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.ListVM(ctx, vmID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Attach 把设备挂到虚拟机上（API-324）。
//
// **要求虚拟机处于关机状态**：直通设备不支持热插拔（有限的支持需要内核与
// 固件的配合，而失败方式是一台机器卡在半启动状态）。用户以为能热插而实际
// 不能时，坏掉的是他那台正在跑的机器。
func (h *Passthrough) Attach(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		Address string `json:"pci_address"`
		Remark  string `json:"remark"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	row, err := h.svc.Attach(ctx, vmID, req.Address, req.Remark,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, row)
}

// Detach 从虚拟机上卸载设备（API-325）。
func (h *Passthrough) Detach(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	address := c.Query("pci_address")
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Detach(ctx, vmID, address, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}
