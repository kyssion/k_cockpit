package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/storage"
)

// StorageVolume 提供存储卷接口（F-5-02）。整体归管理员。
//
// 归管理员的理由与存储池一致：卷会**独占物理设备**，选错盘会影响这台
// 宿主机上所有虚拟机的存储，而那不是租户该有的能力。
type StorageVolume struct {
	svc *storage.Service
}

// NewStorageVolume 构造接口。
func NewStorageVolume(svc *storage.Service) *StorageVolume { return &StorageVolume{svc: svc} }

// List 返回节点上的存储卷（API-140）。
func (h *StorageVolume) List(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.ListVolumes(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type volumeRequest struct {
	Name        string   `json:"name"`
	SizeGB      int      `json:"size_gb"`
	StripeCount int      `json:"stripe_count"`
	MirrorCount int      `json:"mirror_count"`
	Devices     []string `json:"devices"`
}

// Preview 计算这次创建会得到什么（API-141）。
//
// **只读**，不产生任何改动。它回答的是这块功能最容易想错的三个数：
// 需要几块盘、实际占多少物理空间、有没有冗余。
func (h *StorageVolume) Preview(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req volumeRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	plan, err := h.svc.PreviewVolume(ctx, storage.VolumeRequest{
		NodeID: nodeID, Name: req.Name, SizeGB: req.SizeGB,
		StripeCount: req.StripeCount, MirrorCount: req.MirrorCount,
		Devices: req.Devices,
	})
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, plan)
}

type createVolumeRequest struct {
	volumeRequest
	// Acknowledge 表示调用方已看过风险提示并确认继续。
	Acknowledge bool `json:"acknowledge"`
}

// Create 创建存储卷（API-142）。
//
// 有警告而未确认时**不创建、也不报错**，而是把计划原样返回——与防火墙、
// 端口镜像同一套语义。这里的警告主要是「条带没有冗余」，而它是一个几乎
// 所有人都会有的误解：看到「用了 4 块盘」很自然会以为那是 4 块盘的冗余。
func (h *StorageVolume) Create(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req createVolumeRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	plan, t, err := h.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: nodeID, Name: req.Name, SizeGB: req.SizeGB,
		StripeCount: req.StripeCount, MirrorCount: req.MirrorCount,
		Devices: req.Devices,
	}, req.Acknowledge, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"plan": plan, "task": t})
}

// Delete 删除存储卷（API-143）。
//
// **会销毁卷里的全部数据**，且不可恢复。这是它与存储池最不同的一点：
// 池是设备的组织方式，删掉只是"不再用这几块盘"；而卷里装的是真实数据。
func (h *StorageVolume) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "存储卷 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.DeleteVolume(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}
