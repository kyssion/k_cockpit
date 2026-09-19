package handler

import (
	"context"
	"net/url"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
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

	// TemplateID 非零表示从模板克隆（F-3-02）。
	TemplateID int64 `json:"template_id"`
	// CloneMode 取值 full / linked；留空按 full 处理。
	//
	// **链式克隆必须是显式选择**：它引入了「父盘没了数据就没了」这个
	// 依赖，不该是默认行为。
	CloneMode string `json:"clone_mode"`
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
		Name:       req.Name,
		NodeID:     req.NodeID,
		VCPU:       req.VCPU,
		MemoryMB:   req.MemoryMB,
		DiskGB:     req.DiskGB,
		Remark:     req.Remark,
		GroupName:  req.GroupName,
		TemplateID: req.TemplateID,
		CloneMode:  req.CloneMode,
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

type batchActionRequest struct {
	VMIDs  []int64 `json:"vm_ids"`
	Action string  `json:"action"`
	// DiskAction 仅 action=delete 需要，取值 delete / keep。
	//
	// 用指针区分「没传」与「传了空」：批量删除必须显式选择磁盘处理方式
	// （R-009），而缺失与非法是两种不同的错误，文案也不同。
	DiskAction string `json:"disk_action"`
}

// BatchAction 批量操作（API-028）：电源操作与删除。
//
// 响应是**部分成功**语义：逐台给出结果，某一台失败不影响其它台。
// 因此这个接口始终返回 200（除非整个请求就不合法，比如动作拼错了）——
// 用 4xx 会让前端把「50 台里第 3 台状态不允许」当成整批失败。
//
// **批量删除走一次二次验证**（不是每台一次）：用户在确认框里看到的是
// 「删除选中的 12 台」，一次验证对应这一次意图。若每台各验一次，用户会
// 被弹十几次框——那时他会开始机械地输码，验证也就失去了意义。
func (h *VM) BatchAction(ctx context.Context, c *app.RequestContext) {
	var req batchActionRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	if req.Action == vm.BatchActionDelete {
		if !h.risk.Require(c, risk.ActionVMDelete) {
			return
		}
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.Batch(ctx, vm.BatchRequest{
		VMIDs: req.VMIDs, Action: req.Action, DiskAction: req.DiskAction,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

type setLockRequest struct {
	// Locked 用指针区分「没传」与「传了 false」：后者是明确的解锁意图，
	// 当成参数缺失忽略掉会让用户以为解锁成功了。
	Locked *bool  `json:"locked"`
	Reason string `json:"reason"`
}

// SetLock 加锁或解锁（API-067）。
//
// **同步生效，不入队**：锁只存在于控制面，虚拟化层不知道它的存在，
// 因此没有「需要下发才能生效」这回事。
//
// 只有**解锁**需要二次验证（F-2-12）：加锁是收紧、解锁是放松，让收紧
// 也走验证只会让人懒得加锁——而这道锁的价值恰恰在于它被普遍使用。
func (h *VM) SetLock(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req setLockRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.Locked == nil {
		api.Fail(c, api.InvalidParameter("缺少 locked 字段"))
		return
	}

	if !*req.Locked {
		if !h.risk.Require(c, risk.ActionVMLockRelease) {
			return
		}
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.SetLock(ctx, id, vm.LockRequest{
		Locked: *req.Locked, Reason: req.Reason,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
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

// updateConfigRequest 用 map 接收变更，而不是逐个字段。
//
// 接口的形状由**矩阵**决定：新增一个可编辑项时只需要改矩阵，不需要动这里。
// 逐个字段的写法会让新增一项变成三处修改，漏掉任何一处都会让那一项在界面上
// 可改、提交后却被静默丢弃。
type updateConfigRequest struct {
	Changes map[string]any `json:"changes"`
}

// UpdateConfig 提交配置变更（API-058）。
//
// 返回任务标识：这些配置需要下发到节点，且多数要求先关机。
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
		Changes: req.Changes,
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
// 恢复会**丢弃快照之后的所有磁盘改动**，属于不可逆操作，因此走二次验证
// （f-10-02 的清单集中在 internal/risk，此处只声明，不自行判断）。
//
// 注意它与「删除快照」的区别：删除快照只是失去一个还原点，虚拟机当前的数据
// 不受影响，因此**不**需要验证；而恢复是一次真实的回滚。两者在界面上相邻，
// 但危险程度差别很大——这正是清单必须集中管理的原因。
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

	if !h.risk.Require(c, risk.ActionVMSnapshotRestore) {
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

// --- 网络管理的写操作（API-059 ~ API-064）---
//
// 三者都不需要关机（网卡支持热插拔、地址与转发规则是配置层的事），
// 因此受理时不探测运行态——让用户为了加一块网卡去停机是不必要的。
// 全部走任务队列，资源锁键为 vm:<id>，与电源操作串行。

type interfaceRequest struct {
	Model            string `json:"model"`
	SwitchID         *int64 `json:"switch_id"`
	RateLimitMbps    int    `json:"rate_limit_mbps"`
	AllowedAddresses string `json:"allowed_addresses"`
}

// AddInterface 新增网卡（API-059）。
func (h *VM) AddInterface(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req interfaceRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.AddInterface(ctx, id, vm.InterfaceRequest{
		Model: req.Model, SwitchID: req.SwitchID,
		RateLimitMbps: req.RateLimitMbps, AllowedAddresses: req.AllowedAddresses,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// UpdateInterface 修改网卡（API-060）。
func (h *VM) UpdateInterface(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	nicID, err := namedPathID(c, "nicID", "网卡 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req interfaceRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UpdateInterface(ctx, id, nicID, vm.InterfaceRequest{
		Model: req.Model, SwitchID: req.SwitchID,
		RateLimitMbps: req.RateLimitMbps, AllowedAddresses: req.AllowedAddresses,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// RemoveInterface 删除网卡（API-061）。
func (h *VM) RemoveInterface(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	nicID, err := namedPathID(c, "nicID", "网卡 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.RemoveInterface(ctx, id, nicID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

type bindStaticIPRequest struct {
	IP                string `json:"ip"`
	InterfaceOrder    *int   `json:"interface_order"`
	IsDHCPReservation bool   `json:"is_dhcp_reservation"`
}

// BindStaticIP 绑定静态地址（API-062）。
func (h *VM) BindStaticIP(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req bindStaticIPRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.BindStaticIP(ctx, id, vm.BindStaticIPRequest{
		IP:                req.IP,
		InterfaceOrder:    req.InterfaceOrder,
		IsDHCPReservation: req.IsDHCPReservation,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// UnbindStaticIP 解绑静态地址（API-063）。
func (h *VM) UnbindStaticIP(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	ipID, err := namedPathID(c, "ipID", "静态地址 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UnbindStaticIP(ctx, id, ipID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// PortForwards 返回端口转发列表（API-064）。
func (h *VM) PortForwards(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.PortForwards(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type addPortForwardRequest struct {
	Protocol   string `json:"protocol"`
	HostPort   int    `json:"host_port"`
	TargetIP   string `json:"target_ip"`
	TargetPort int    `json:"target_port"`
	AllowedIPs string `json:"allowed_ips"`
}

// AddPortForward 新增端口转发（API-065）。
func (h *VM) AddPortForward(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req addPortForwardRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.AddPortForward(ctx, id, vm.AddPortForwardRequest{
		Protocol: req.Protocol, HostPort: req.HostPort,
		TargetIP: req.TargetIP, TargetPort: req.TargetPort,
		AllowedIPs: req.AllowedIPs,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// RemovePortForward 删除端口转发（API-066）。
func (h *VM) RemovePortForward(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	pfID, err := namedPathID(c, "pfID", "转发规则 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.RemovePortForward(ctx, id, pfID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// EnterRescue 让虚拟机从救援镜像启动（API-074 / F-2-12）。
//
// 不需要二次验证：救援**不破坏数据**（它只改引导与盘型，磁盘内容原样保留，
// 退出时还原），而且它恰恰是用户遇到故障时的出路——给出路加验证，只会让人
// 在系统已经出问题的时候再被挡一道。
func (h *VM) EnterRescue(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.EnterRescue(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// ExitRescue 退出救援并按进入前的快照还原配置（API-075 / F-2-12）。
func (h *VM) ExitRescue(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.ExitRescue(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

type reinstallRequest struct {
	// TemplateID 是要用来重建系统盘的模板。
	TemplateID int64 `json:"template_id"`
}

// Reinstall 重装系统（API-081 / F-2-11）。
//
// **高风险，需二次验证**：整块系统盘会被替换，原系统上的软件与配置全部消失
// （数据盘保留）。这是本项目里对单台虚拟机破坏性最强的操作——比删除轻一档，
// 但同样不可逆。
func (h *VM) Reinstall(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req reinstallRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.TemplateID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定用于重装的模板"))
		return
	}

	if !h.risk.Require(c, risk.ActionVMReinstall) {
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Reinstall(ctx, id, req.TemplateID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// PurgeReinstallBackup 清理重装留下的备份盘（API-082 / F-2-11）。
//
// **不需要二次验证**：删掉的是已经不再被使用的备份，虚拟机的当前运行不受
// 任何影响。给它加验证只会稀释真正危险操作的份量。
func (h *VM) PurgeReinstallBackup(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.PurgeBackup(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

type createExportRequest struct {
	// Format 取值 qcow2 / ova。留空按 qcow2 处理。
	Format string `json:"format"`
	// IncludeDataDisks 是否连同数据盘一起导出；**默认不包含**——
	// 数据盘可能远大于系统盘，而多数导出是为了复用系统环境，不是搬数据。
	IncludeDataDisks bool `json:"include_data_disks"`
}

// Exports 返回虚拟机的导出记录（API-084）。
func (h *VM) Exports(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.ListExports(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// CreateExport 受理一次导出（API-084 / F-2-14）。
func (h *VM) CreateExport(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req createExportRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.Format == "" {
		req.Format = model.ExportQCOW2
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Export(ctx, id, vm.ExportRequest{
		Format: req.Format, IncludeDataDisks: req.IncludeDataDisks,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// DeleteExport 删除一个导出产物（API-085）。
func (h *VM) DeleteExport(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	exportID, err := namedPathID(c, "exportID", "导出 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.DeleteExport(ctx, vmID, exportID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// DownloadExport 下载导出产物（API-086）。
//
// 直接返回**产物字节**而不是一个节点上的直链：直链意味着要把节点的访问凭据
// 或一个匿名可访问的地址暴露出去，而产物里是整台机器的数据。控制面转发多花
// 一次带宽，但权限判断留在一处。
func (h *VM) DownloadExport(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	exportID, err := namedPathID(c, "exportID", "导出 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	name, data, mime, err := h.svc.ExportFile(ctx, vmID, exportID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 产物可能很大，且一旦生成就不再变化，因此**允许**浏览器缓存——与
	// 控制台截帧相反（那是「此刻的样子」，缓存住会误导）。
	c.Header("Cache-Control", "private, max-age=3600")
	// 中文文件名要用 RFC 5987 的 filename* 形式，否则浏览器会得到乱码。
	c.Header("Content-Disposition", contentDisposition(name))
	c.SetContentType(mime)
	c.Response.SetBody(data)
}

type guestRequest struct {
	Action   string `json:"action"`
	Username string `json:"username"`
	Password string `json:"password"`
	DiskID   string `json:"disk_id"`
	DiskGB   int    `json:"disk_gb"`
}

// GuestCapabilities 返回当前状态下可用的来宾自动化动作（API-088）。
func (h *VM) GuestCapabilities(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	view, err := h.svc.GuestCapabilities(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// GuestAction 受理一次来宾自动化（API-089 / F-2-10）。
//
// 四种动作共用这一个入口：它们都要「先做宿主机侧的准备、再进来宾执行」，
// 骨架一致，差异只在具体命令上。拆成四个接口会让四份几乎相同的受理逻辑
// 各自演化。
func (h *VM) GuestAction(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req guestRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, view, err := h.svc.GuestAction(ctx, id, vm.GuestRequest{
		Action: req.Action, Username: req.Username, Password: req.Password,
		DiskID: req.DiskID, DiskGB: req.DiskGB,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{
		"task_id": t.ID, "status": t.Status, "capabilities": view,
	})
}

type migrateRequest struct {
	ToNodeID int64 `json:"to_node_id"`
}

// Migrate 受理一次跨节点迁移（API-098 / F-2-09）。
//
// **不需要二次验证**：迁移的可逆性在于源侧数据在目标侧确认之前不删——
// 失败时源侧保留完整的数据，虚拟机在那里仍然可用。它不是不可逆操作，
// 而给可逆操作加验证只会稀释验证本身的分量。
func (h *VM) Migrate(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req migrateRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Migrate(ctx, id, vm.MigrateRequest{ToNodeID: req.ToNodeID},
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// Migrations 返回虚拟机的迁移记录（API-099）。
//
// 界面据此回答「这台机器原来在哪台宿主机上」——那是排查存储、网络、性能
// 问题时第一条要看的东西，而 vm.node_id 只记录了「现在在哪」。
func (h *VM) Migrations(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.ListMigrations(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Stats 返回虚拟机的实时运行指标（Hero 的资源卡）。
//
// 响应里带 `at`（采集时刻）：指标是瞬时值，轮询失败时界面会继续显示上一组
// 数字，没有采集时刻就分不清「当前」与「几分钟前」。
func (h *VM) Stats(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	stats, err := h.svc.Stats(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, stats)
}

// ConsoleFrame 返回一帧控制台画面（Hero 的控制台预览卡）。
//
// 直接返回 **image/png 字节**而不是包在 JSON 里：画面是二进制，塞进 JSON
// 要 base64 编码（体积涨 33%）再由前端解码成 data URL，两条路径都不产生
// 额外信息，只是多绕一圈。用图片本身也让浏览器能正常缓存与懒加载。
func (h *VM) ConsoleFrame(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	frame, err := h.svc.ConsoleFrame(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 画面不进浏览器缓存：它是「此刻的样子」，缓存住会让用户看着一张
	// 几分钟前的图，还以为虚拟机画面卡住了。
	c.Header("Cache-Control", "no-store")
	c.SetContentType(frame.MIME)
	c.Response.SetBody(frame.Data)
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

// contentDisposition 构造下载响应头，兼容非 ASCII 文件名。
//
// 分两段写是 RFC 6266 / 5987 的要求：老客户端只认 ASCII 的 filename，
// 新客户端优先用 filename*。**只写 filename** 会让中文/空格文件名变成乱码；
// **只写 filename*** 会让老客户端拿到一个没有文件名、叫 "download" 的落盘
// 文件——两边各丢一半，因此两段都要给。
func contentDisposition(name string) string {
	// ASCII 兜底：非 ASCII 字符替换成下划线。替换而不是丢弃，是为了让
	// 兜底文件名仍然保留长度与大致形状，用户至少能分辨是哪个文件。
	var ascii strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii.WriteByte('_')
			continue
		}
		ascii.WriteRune(r)
	}
	fallback := ascii.String()
	if fallback == "" {
		fallback = "download"
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` +
		url.PathEscape(name)
}

type resizeDiskRequest struct {
	SizeGB int `json:"size_gb"`
}

// ResizeDisk 把虚拟机的系统盘扩容（API-029）。
//
// **只能扩，不能缩**：缩容会丢数据——镜像文件变小之后，文件系统里超出新边界
// 的那些块还在原地，但已经不属于这个设备了，而文件系统自己不知道。这不是
// 「有风险」，是「一定会坏」。
//
// **要求关机**：运行中的扩容请走「来宾自动化」里的「扩容磁盘」，那条路会在
// 扩完之后顺带在来宾里扩好文件系统。两条路径各做一半的话，用户得到的是
// 「盘大了但用不了」——正是这个功能要消灭的那种机器。
func (h *VM) ResizeDisk(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req resizeDiskRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.ResizeDisk(ctx, id, req.SizeGB, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// MakeDisksIndependent 把链接克隆的磁盘变成独立盘（API-290）。
//
// **它存在的理由是一件事：链接克隆的父盘删不掉。** 链接克隆的磁盘只是模板
// 之上的一层覆盖，这让克隆很快、很省空间，代价是那台机器永远依赖着父盘。
// 而模板的管理（更新、清理、下线）需要能删掉旧的父盘。
func (h *VM) MakeDisksIndependent(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.MakeDisksIndependent(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

type batchCloneRequest struct {
	NamePrefix string `json:"name_prefix"`
	Count      int    `json:"count"`
	NodeID     int64  `json:"node_id"`
	VCPU       int    `json:"vcpu"`
	MemoryMB   int    `json:"memory_mb"`
	DiskGB     int    `json:"disk_gb"`
	TemplateID int64  `json:"template_id"`
	CloneMode  string `json:"clone_mode"`
	GroupName  string `json:"group_name"`
	Remark     string `json:"remark"`
}

// BatchClone 一次克隆多台虚拟机（API-291）。
//
// **一次最多 5 台**，理由在服务层的报错里写明：每台都要完整读一遍父盘再写
// 一份新的，同时进行的台数越多，宿主机上的存储被占得越久——表现为**所有**
// 虚拟机的 IO 都变慢，而用户很难把它和「我刚才点了克隆」联系起来。
//
// 这一批名称**整批先查重**（逐台跳过重名会建出带洞的结果），而**部分失败
// 如实报告且不回滚**——第 3 台失败时前两台已经建好了，撤销意味着删掉可能
// 已经分发出去了的机器。
func (h *VM) BatchClone(ctx context.Context, c *app.RequestContext) {
	var req batchCloneRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.BatchClone(ctx, vm.BatchCloneRequest{
		CreateRequest: vm.CreateRequest{
			NodeID: req.NodeID, VCPU: req.VCPU, MemoryMB: req.MemoryMB,
			DiskGB: req.DiskGB, TemplateID: req.TemplateID, CloneMode: req.CloneMode,
			GroupName: req.GroupName, Remark: req.Remark,
		},
		NamePrefix: req.NamePrefix,
		Count:      req.Count,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// CDROMs 返回虚拟机的光驱（API-292）。
func (h *VM) CDROMs(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.ListCDROMs(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// AttachCDROM 加一个光驱（API-293）。
func (h *VM) AttachCDROM(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		ISOFileID int64  `json:"iso_file_id"`
		Bus       string `json:"bus"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.AttachCDROM(ctx, id, req.ISOFileID, req.Bus,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// LoadCDROM 给光驱放盘或换盘（API-294）。
//
// 换盘之后**来宾通常看不到新介质**（多数系统缓存了介质信息），界面上要
// 提示这一点——否则用户会以为换盘失败而反复重试。
func (h *VM) LoadCDROM(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	cdromID, err := namedPathID(c, "cdromID", "光驱 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		ISOFileID int64 `json:"iso_file_id"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.LoadCDROM(ctx, id, cdromID, req.ISOFileID,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// EjectCDROM 弹出光盘（**光驱保留**）（API-295）。
func (h *VM) EjectCDROM(ctx context.Context, c *app.RequestContext) {
	h.cdromAction(ctx, c, "eject")
}

// RemoveCDROM 摘掉整个光驱（API-296）。
func (h *VM) RemoveCDROM(ctx context.Context, c *app.RequestContext) {
	h.cdromAction(ctx, c, "remove")
}

// SetCDROMBus 换总线类型（API-297）。**通常需要重启才生效。**
func (h *VM) SetCDROMBus(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	cdromID, err := namedPathID(c, "cdromID", "光驱 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		Bus string `json:"bus"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.SetCDROMBus(ctx, id, cdromID, req.Bus,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

func (h *VM) cdromAction(ctx context.Context, c *app.RequestContext, action string) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	cdromID, err := namedPathID(c, "cdromID", "光驱 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	var t any
	if action == "eject" {
		t, err = h.svc.EjectCDROM(ctx, id, cdromID, authz.ViewerOf(c), user.Username, info.IP)
	} else {
		t, err = h.svc.RemoveCDROM(ctx, id, cdromID, authz.ViewerOf(c), user.Username, info.IP)
	}
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// PreviewMigration 迁移预检（API-298）。**只读**，不产生任何记录。
//
// 它要回答用户点下按钮**之前**唯一想知道的那件事：**这次要停多久。**
// 因此除了「能不能迁」，还必须给出**会怎么迁**（停机迁移 + 要复制的数据量）
// ——停机时长由后者决定，而不是由前者决定。
//
// **与 Migrate 共用同一套校验**：分两处写迟早会分叉，而分叉的表现是最难
// 解释的一种——预览说可以，点下去被拒。用户会反复确认自己的操作，而问题
// 在于两处用了不同的规则。
func (h *VM) PreviewMigration(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		ToNodeID int64 `json:"to_node_id"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	view, err := h.svc.PreviewMigration(ctx, id, req.ToNodeID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// BatchRemovePortForwards 批量删除端口转发（API-302）。
func (h *VM) BatchRemovePortForwards(ctx context.Context, c *app.RequestContext) {
	var req struct {
		Items []vm.PortForwardRef `json:"items"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	res, err := h.svc.RemovePortForwards(ctx, req.Items,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, res)
}
