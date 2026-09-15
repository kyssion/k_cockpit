package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/vm"
)

// VM 提供虚拟机接口（F-2-01 ~ F-2-04）。
//
// 权限：管理员可操作全部；tenant 只能操作自己名下的（f-1-06 §3.1）。
// 该判定由**归属过滤**在数据访问层强制注入，不在本文件重复实现。
type VM struct {
	svc *vm.Service
}

// NewVM 构造虚拟机接口。
func NewVM(svc *vm.Service) *VM {
	return &VM{svc: svc}
}

// List 返回虚拟机列表。
func (h *VM) List(ctx context.Context, c *app.RequestContext) {
	page, pageSize := pageParams(c)

	items, total, err := h.svc.List(ctx, vm.ListFilter{
		Status:    c.Query("status"),
		Keyword:   c.Query("keyword"),
		NodeID:    int64(queryInt(c, "node_id")),
		GroupName: c.Query("group_name"),
		Page:      page,
		PageSize:  pageSize,
		Viewer:    authz.ViewerOf(c),
	})
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OKPage(c, items, api.NewPage(page, pageSize, total))
}

// Get 返回虚拟机详情。
func (h *VM) Get(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	view, err := h.svc.Get(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type createVMRequest struct {
	Name      string `json:"name"`
	NodeID    int64  `json:"node_id"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	Remark    string `json:"remark"`
	GroupName string `json:"group_name"`
}

// Create 创建虚拟机。
//
// 返回 **202 与任务标识**而不是创建好的虚拟机：创建涉及磁盘镜像复制等
// 耗时操作，同步等待会让请求超时（f-7-01 R-001）。前端据任务标识跟踪进度。
//
// 请求体中的 `owner_id` 一律**忽略**，归属以当前用户为准（f-1-06 R-007）——
// 接受前端传入的归属等于把越权写入交给客户端控制。
func (h *VM) Create(ctx context.Context, c *app.RequestContext) {
	var req createVMRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Create(ctx, vm.CreateRequest{
		Name:      req.Name,
		NodeID:    req.NodeID,
		VCPU:      req.VCPU,
		MemoryMB:  req.MemoryMB,
		DiskGB:    req.DiskGB,
		Remark:    req.Remark,
		GroupName: req.GroupName,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{
		"task_id": t.ID,
		"status":  t.Status,
	})
}

type powerVMRequest struct {
	// Action 取值 start / shutdown / reboot / poweroff / reset。
	Action string `json:"action"`
}

// Power 执行电源操作（F-2-04）。
//
// 返回任务标识而非执行结果：电源操作可能耗时（优雅关机要等来宾响应），
// 同步等待会让请求超时（f-7-01 R-001）。
//
// 状态合法性由服务端基于**实时探测**判定，不信任前端传来的任何状态
// （f-2-01 R-004：前端禁用只是体验优化，不构成安全边界）。
func (h *VM) Power(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req powerVMRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	// 兼容 query 传参：路径上已有 :id，动作写在 body 里对某些客户端不顺手。
	if req.Action == "" {
		req.Action = c.Query("action")
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Power(ctx, id, req.Action, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{
		"task_id": t.ID,
		"status":  t.Status,
	})
}

type deleteVMRequest struct {
	// DiskAction 必填：delete（连同磁盘删除）/ keep（保留磁盘）。
	// 服务端**不设默认值**（f-2-01 R-009）。
	DiskAction string `json:"disk_action"`
}

// Delete 删除虚拟机（F-2-04）。
//
// 磁盘处理方式由用户在界面上显式选择后传入；缺失时服务端拒绝，而不是
// 替用户选一个——默认连盘删除的误操作代价是数据永久丢失。
func (h *VM) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req deleteVMRequest
	// DELETE 带 body 并非所有客户端都支持，解析失败不直接拒绝，
	// 继续尝试 query（必填语义由服务层保证）。
	_ = c.Bind(&req)
	if req.DiskAction == "" {
		req.DiskAction = c.Query("disk_action")
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Delete(ctx, id, vm.DeleteRequest{DiskAction: req.DiskAction},
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{
		"task_id": t.ID,
		"status":  t.Status,
	})
}
