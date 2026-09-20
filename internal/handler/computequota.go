package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/computequota"
)

// ComputeQuota 提供计算资源配额的设置与查看（vCPU / 内存 / 实例数）。
type ComputeQuota struct {
	svc *computequota.Service
}

// NewComputeQuota 构造接口。
func NewComputeQuota(svc *computequota.Service) *ComputeQuota {
	return &ComputeQuota{svc: svc}
}

// List 列出某节点上各用户的配额与占用。
func (h *ComputeQuota) List(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.List(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type computeQuotaRequest struct {
	NodeID       int64 `json:"node_id"`
	UserID       int64 `json:"user_id"`
	VCPU         int   `json:"vcpu"`
	MemoryMB     int   `json:"memory_mb"`
	VMCount      int   `json:"vm_count"`
	Snapshots    int   `json:"snapshots"`
	PortForwards int   `json:"port_forwards"`
	PublicIPs    int   `json:"public_ips"`
}

// Set 设置配额。六个上限全为 0 表示删除（回到不限）。
func (h *ComputeQuota) Set(ctx context.Context, c *app.RequestContext) {
	var req computeQuotaRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	err := h.svc.Set(ctx, req.NodeID, req.UserID, computequota.Limits{
		VCPU: req.VCPU, MemoryMB: req.MemoryMB, VMCount: req.VMCount,
		Snapshots: req.Snapshots, PortForwards: req.PortForwards, PublicIPs: req.PublicIPs,
	}, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}
