package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/vm"
)

// VM 提供虚拟机接口（F-2-01 ~ F-2-04）。
//
// 权限：管理员可操作全部；tenant 只能操作自己名下的（f-1-06 §3.1）。
// 该判定由**归属过滤**在数据访问层强制注入，不在本文件重复实现。
type VM struct {
	svc  *vm.Service
	risk *risk.Guard
}

// NewVM 构造虚拟机接口。
func NewVM(svc *vm.Service, guard *risk.Guard) *VM {
	return &VM{svc: svc, risk: guard}
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
	// 高风险操作：删除（尤其连盘删除）会造成不可逆结果（f-10-02）。
	//
	// 检查放在解析参数之前：验证未通过时不做任何业务处理，也不消费任何
	// 业务资源。
	if !h.risk.Require(c, risk.ActionVMDelete) {
		return
	}

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

// Interfaces 返回虚拟机的网卡列表（F-2-03「网络管理」标签页）。
//
// 只读、不分页：一台虚拟机的网卡是个位数，分页只会让前端多写一层处理。
func (h *VM) Interfaces(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.Interfaces(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// EditForm 返回编辑页的表单元数据与当前值（API-056）。
//
// 返回值里的 **运行态可改矩阵**是单一事实来源（F-2-05）：界面按它渲染控件与
// 「可热改 / 需关机」标记，后端按它校验提交。前端不得硬编码第二份——两份
// 规则漂移的表现是「界面上能改、提交后被拒」，而只有真正动手的用户才会碰到。
func (h *VM) EditForm(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	form, err := h.svc.EditFormOf(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, form)
}

type updateMetadataRequest struct {
	// 用指针区分「没提交这一项」与「提交了空值」：后者是用户明确要清空备注。
	Remark    *string `json:"remark"`
	GroupName *string `json:"group_name"`
}

// UpdateMetadata 修改备注、分组（API-057）。
//
// **同步返回，不入队**：这两项是纯控制面元数据，虚拟化层不知道它们的存在，
// 因此没有「运行中不能改」这回事。让它们和硬件配置走同一条路径，会让改个
// 备注也要等一次节点往返。
func (h *VM) UpdateMetadata(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req updateMetadataRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.UpdateMetadata(ctx, id, vm.UpdateMetadataRequest{
		Remark:    req.Remark,
		GroupName: req.GroupName,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type updateConfigRequest struct {
	VCPU     *int `json:"vcpu"`
	MemoryMB *int `json:"memory_mb"`
}

// UpdateConfig 提交硬件配置变更（API-058）。
//
// 返回任务标识：修改 CPU / 内存需要下发到节点，且可能要求先关机。
func (h *VM) UpdateConfig(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req updateConfigRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UpdateConfig(ctx, id, vm.ConfigChangeRequest{
		VCPU:     req.VCPU,
		MemoryMB: req.MemoryMB,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// Snapshots 返回虚拟机的快照列表与配额（API-052）。
func (h *VM) Snapshots(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	list, err := h.svc.Snapshots(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, list)
}

type createSnapshotRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// IncludeMemory 表示是否保存运行现场（含内存）。**不做默认值猜测**：
	// 含内存的快照体积可能数倍于磁盘，是否值得由用户判断。
	IncludeMemory bool `json:"include_memory"`
}

// CreateSnapshot 创建快照（API-053）。
//
// 返回任务标识而非执行结果：创建要复制整个磁盘镜像，可能耗时数分钟，
// 同步等待会让请求超时（f-7-01 R-001）。
func (h *VM) CreateSnapshot(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req createSnapshotRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.CreateSnapshot(ctx, id, vm.CreateSnapshotRequest{
		Name:          req.Name,
		Description:   req.Description,
		IncludeMemory: req.IncludeMemory,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// RestoreSnapshot 恢复快照（API-054）。
//
// 恢复会**丢弃快照之后的所有磁盘改动**，属于不可逆操作。当前未纳入高风险
// 清单（f-10-02 的清单集中在 internal/risk），因此没有二次验证——这一点
// 是已知缺口，登记在 API.md 中。
func (h *VM) RestoreSnapshot(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	snapshotID, err := namedPathID(c, "snapshotID", "快照 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.RestoreSnapshot(ctx, id, snapshotID,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// DeleteSnapshot 删除快照（API-055）。
func (h *VM) DeleteSnapshot(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	snapshotID, err := namedPathID(c, "snapshotID", "快照 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.DeleteSnapshot(ctx, id, snapshotID,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// StaticIPs 返回虚拟机的静态地址列表。
func (h *VM) StaticIPs(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.StaticIPs(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}
