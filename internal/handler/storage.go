package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/storage"
)

// Storage 提供存储池接口（F-5-01）。
//
// 权限：存储池管理为 **admin 专属**（f-5-01 R-011 / f-1-06 §3.1）。该要求
// 在路由注册时声明，不在本文件内判断。
type Storage struct {
	svc  *storage.Service
	risk *risk.Guard
}

// NewStorage 构造存储池接口。
func NewStorage(svc *storage.Service, guard *risk.Guard) *Storage {
	return &Storage{svc: svc, risk: guard}
}

// Disks 返回节点的块设备清单（API-044）。
func (h *Storage) Disks(ctx context.Context, c *app.RequestContext) {
	nodeID, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	disks, err := h.svc.Disks(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, disks)
}

// ListPools 返回节点的存储池列表（API-045）。
func (h *Storage) ListPools(ctx context.Context, c *app.RequestContext) {
	nodeID, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	pools, err := h.svc.List(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, pools)
}

// GetPool 返回单个存储池。
func (h *Storage) GetPool(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "存储池 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	pool, err := h.svc.Get(ctx, id)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, pool)
}

type createPoolRequest struct {
	NodeID   int64  `json:"node_id"`
	DeviceID string `json:"device_id"`
	FSType   string `json:"fs_type"`
	// ConfirmDeviceName 必须与设备路径完全一致（f-5-01 R-004）。
	ConfirmDeviceName string `json:"confirm_device_name"`
	// ConfirmDataLoss 在设备已有数据时必须为 true。
	ConfirmDataLoss bool `json:"confirm_data_loss"`
	IsDefault       bool `json:"is_default"`
}

// CreatePool 创建存储池（API-046）。
//
// 格式化**不可逆**，因此这是受二次验证保护的操作（f-5-01 R-004）。两道
// 防线叠加而非二选一：二次验证确认「是你本人在操作」，设备名确认确认
// 「你清楚目标是哪块盘」——只有前者的话，用户仍然可能在确认框里点快了。
func (h *Storage) CreatePool(ctx context.Context, c *app.RequestContext) {
	if !h.risk.Require(c, risk.ActionStoragePoolCreate) {
		return
	}

	var req createPoolRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Create(ctx, storage.CreateRequest{
		NodeID:            req.NodeID,
		DeviceID:          req.DeviceID,
		FSType:            req.FSType,
		IsDefault:         req.IsDefault,
		ConfirmDeviceName: req.ConfirmDeviceName,
		ConfirmDataLoss:   req.ConfirmDataLoss,
	}, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

type updatePoolRequest struct {
	IsDefault *bool `json:"is_default"`
}

// UpdatePool 设为默认池（API-047）。
//
// **同步完成**：它只改控制面元数据，不触碰宿主机。做成异步任务会让用户
// 点一下「设为默认」还要去任务中心看结果。
func (h *Storage) UpdatePool(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "存储池 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req updatePoolRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.IsDefault == nil {
		api.Fail(c, api.InvalidParameter("缺少要修改的字段"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	pool, err := h.svc.SetDefault(ctx, id, *req.IsDefault, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, pool)
}

// DeletePool 删除存储池（API-048）。
//
// 受二次验证保护：删除存储池会销毁其中的磁盘，属不可逆操作。
func (h *Storage) DeletePool(ctx context.Context, c *app.RequestContext) {
	if !h.risk.Require(c, risk.ActionStoragePoolDelete) {
		return
	}

	id, err := namedPathID(c, "id", "存储池 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Delete(ctx, id, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
