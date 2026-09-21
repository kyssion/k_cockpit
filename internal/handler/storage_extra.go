package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/storage"
)

// --- 分区 ---

type partitionRequest struct {
	NodeID   int64  `json:"node_id"`
	DeviceID string `json:"device_id"`
	SizeGB   int    `json:"size_gb"`
	Index    int    `json:"index"`
	All      bool   `json:"all"`
}

// Partitions 列出一块磁盘上的分区。
func (h *Storage) Partitions(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	deviceID := c.Query("device_id")
	if nodeID <= 0 || deviceID == "" {
		api.Fail(c, api.InvalidParameter("必须指定节点与磁盘"))
		return
	}
	view, err := h.svc.Partitions(ctx, nodeID, deviceID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// CreatePartition 创建分区。改分区表是**不可逆**的，因此要二次验证。
func (h *Storage) CreatePartition(ctx context.Context, c *app.RequestContext) {
	if !h.risk.Require(c, risk.ActionStoragePartitionCreate) {
		return
	}
	var req partitionRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.CreatePartition(ctx, storage.PartitionRequest{
		NodeID: req.NodeID, DeviceID: req.DeviceID, SizeGB: req.SizeGB,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// DeletePartitions 删除分区。删全部比删一个严重得多，因此**分别登记**为两个
// 高风险动作：理由不同，二次验证要让用户看到的是他正在做的那件事。
func (h *Storage) DeletePartitions(ctx context.Context, c *app.RequestContext) {
	var req partitionRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	action := risk.ActionStoragePartitionDelete
	if req.All {
		action = risk.ActionStoragePartitionDeleteAll
	}
	if !h.risk.Require(c, action) {
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.DeletePartitions(ctx, storage.PartitionRequest{
		NodeID: req.NodeID, DeviceID: req.DeviceID, Index: req.Index, All: req.All,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// --- 池配置 / 卸载 ---

type poolConfigRequest struct {
	MountPath *string `json:"mount_path"`
	AutoMount *bool   `json:"auto_mount"`
	Remark    *string `json:"remark"`
}

// UpdatePoolConfig 下发池配置（挂载点 / 开机自动挂载 / 备注）。
//
// 它与 PATCH（设为默认）分开：前者要动宿主机上的挂载与 fstab，后者只是改
// 控制面的一列。混在一个"保存"里会让不同字段有完全不同的耗时与失败语义。
func (h *Storage) UpdatePoolConfig(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "存储池 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req poolConfigRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UpdatePoolConfig(ctx, id, storage.PoolConfigRequest{
		MountPath: req.MountPath, AutoMount: req.AutoMount, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// UnmountPool 卸载存储池（保留数据）。
func (h *Storage) UnmountPool(ctx context.Context, c *app.RequestContext) {
	if !h.risk.Require(c, risk.ActionStoragePoolUnmount) {
		return
	}
	id, err := namedPathID(c, "id", "存储池 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UnmountPool(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// --- trim ---

// TrimStorage 对节点上的块设备下发 trim / discard。
func (h *Storage) TrimStorage(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.TrimStorage(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}
