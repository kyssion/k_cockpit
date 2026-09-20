// Package publicip 实现公网 IP 的录入、绑定与浮动迁移（F-4-06）。
//
// 两条贯穿本包的设计：
//
//  1. **地址是节点内唯一的资源**。同一个地址在两台宿主机上出现会让流量
//     随机落到其中一台，而那种问题从现象上几乎看不出来——因此唯一性由
//     数据库索引兜底，服务层再给出能看懂的话。
//
//  2. **一个地址同一时刻只有一条有效绑定**。这是网络层的事实，不是控制面
//     的偏好。因此浮动迁移（faultover 的实现方式）**必须是原子的
//     「解绑旧、绑定新」**，而不能是「再加一条绑定」——后者会被数据库直接
//     拒绝，而如果绕过索引去实现，就会得到两个地方同时宣告同一个地址。
package publicip

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Service 提供公网 IP 能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
	// computeQuota 校验"这个用户还能再占几个公网地址"。可选：未装配即
	// 不校验，与虚拟机侧的做法一致。
	computeQuota Checker
}

// Checker 是计算配额校验的最小接口。
//
// 在本包声明而不是直接依赖 computequota：本包只需要"能不能再加一个"这一
// 件事，让接口跟着调用方定义，可以避免把配额包的所有方法都牵进来。
type Checker interface {
	Check(ctx context.Context, userID, nodeID int64, add computequota.Additions) error
}

// NewService 构造公网 IP 服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder}
}

// SetComputeQuota 装配计算配额校验器；不调用即不校验。
//
// 用 setter 而不是构造参数：现有的调用点（含大量测试）并不关心配额，
// 为一个可选能力改动全部签名不值得。
func (s *Service) SetComputeQuota(c Checker) { s.computeQuota = c }

// View 是一个公网地址的对外视图（含当前的绑定情况）。
type View struct {
	ID       int64  `gorm:"column:id" json:"id"`
	NodeID   int64  `json:"node_id"`
	IP       string `json:"ip"`
	CIDR     string `json:"cidr,omitempty"`
	Gateway  string `json:"gateway,omitempty"`
	EgressIf string `json:"egress_if,omitempty"`

	AddressFamily string `json:"address_family"`
	// SupportedModes 该地址支持哪些绑定模式（f-4-06）。
	SupportedModes []string `json:"supported_modes"`
	Status         string   `json:"status"`
	Remark         string   `json:"remark,omitempty"`

	// --- 当前绑定；无绑定时全部为空 ---

	BindingID *int64 `json:"binding_id,omitempty"`
	VMID      *int64 `json:"vm_id,omitempty"`
	// VMName 供界面直接显示「这个地址指向哪台机器」。
	//
	// 只给 VMID 的话，界面为了显示一个名字还得再查一次虚拟机列表——
	// 而那一页上要显示几十个地址。
	VMName string `json:"vm_name,omitempty"`
	Mode   string `json:"mode,omitempty"`
	// RuntimeStatus 是节点上的实际生效状态；见 model.BindingPending 等。
	RuntimeStatus string `json:"runtime_status,omitempty"`
	BoundAt       string `json:"bound_at,omitempty"`
}

// List 返回节点上的公网地址池。
func (s *Service) List(ctx context.Context, nodeID int64) ([]View, error) {
	query := s.db.WithContext(ctx).Model(&model.PublicIP{}).Order("ip ASC")
	if nodeID > 0 {
		query = query.Where("node_id = ?", nodeID)
	}

	var rows []model.PublicIP
	if err := query.Find(&rows).Error; err != nil {
		log.Printf("[publicip] 查询地址池失败: %v", err)
		return nil, api.Internal()
	}
	if len(rows) == 0 {
		return []View{}, nil
	}

	// 绑定与虚拟机名**一次查完**：逐个查会把一次列表请求变成上百次往返，
	// 而地址池可能很大（一个 /24 就是 254 条）。
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}

	var bindings []model.PublicIPBinding
	if err := s.db.WithContext(ctx).
		Where("public_ip_id IN ? AND released_at IS NULL", ids).
		Find(&bindings).Error; err != nil {
		log.Printf("[publicip] 查询绑定失败: %v", err)
		return nil, api.Internal()
	}
	byIP := make(map[int64]*model.PublicIPBinding, len(bindings))
	vmIDs := make([]int64, 0, len(bindings))
	for i := range bindings {
		byIP[bindings[i].PublicIPID] = &bindings[i]
		if bindings[i].VMID != nil {
			vmIDs = append(vmIDs, *bindings[i].VMID)
		}
	}

	vmNames := map[int64]string{}
	if len(vmIDs) > 0 {
		var vms []model.VM
		if err := s.db.WithContext(ctx).Select("id", "name").
			Where("id IN ?", vmIDs).Find(&vms).Error; err != nil {
			log.Printf("[publicip] 查询虚拟机名失败: %v", err)
			return nil, api.Internal()
		}
		for i := range vms {
			vmNames[vms[i].ID] = vms[i].Name
		}
	}

	views := make([]View, 0, len(rows))
	for i := range rows {
		views = append(views, toView(&rows[i], byIP[rows[i].ID], vmNames))
	}
	return views, nil
}

// CreateRequest 是录入地址的请求（f-4-06：单条或批量）。
type CreateRequest struct {
	NodeID int64
	// IP 可以是单个地址，也可以是 CIDR（如 203.0.113.0/24）。
	IP       string
	Gateway  string
	EgressIf string
	// SupportedModes 留空表示三种模式都支持。
	SupportedModes []string
	Remark         string
}

// Create 录入地址。
//
// **支持 CIDR 批量展开**，但有一个刻意的限制：网络地址、广播地址与网关
// 会被跳过。它们不能被分配给虚拟机——展开时不过滤的话，用户会得到一个
// 看起来正常、绑定时才失败的地址，而失败信息来自内核（「无法分配」），
// 与他刚才做的事联系不起来。
//
// **同步生效、不入队**：录入只是往控制面的地址池里写记录，宿主机上什么都
// 还没发生。真正的网络改动在绑定时才发生。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, v authz.Viewer, operatorName, clientIP string,
) ([]View, error) {
	if req.NodeID <= 0 {
		return nil, api.InvalidParameter("必须指定节点")
	}
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}

	ips, err := expand(req.IP, req.Gateway)
	if err != nil {
		return nil, err
	}
	modes := req.SupportedModes
	for _, m := range modes {
		if !validMode(m) {
			return nil, api.InvalidParameter("不支持的绑定模式：" + m)
		}
	}
	// 校验 IPv6 与 NAT 的组合：IPv6 地址充足，不需要地址转换，
	// 而上游通常也不会为它配一条 NAT 规则。允许这个组合会让绑定
	// 在节点上以一句内核报错结束。
	if isIPv6(ips[0]) {
		for _, m := range modes {
			if m == model.PublicIPModeNAT {
				return nil, api.InvalidParameter(
					"IPv6 地址不支持 1:1 NAT —— 地址本就充足，直接路由即可")
			}
		}
	}

	var modePtr *string
	if len(modes) > 0 {
		joined := strings.Join(modes, ",")
		modePtr = &joined
	}

	created := make([]model.PublicIP, 0, len(ips))
	for _, ip := range ips {
		row := model.PublicIP{
			NodeID: req.NodeID, IP: ip,
			AddressFamily:  familyOf(ip),
			Status:         model.PublicIPAvailable,
			SupportedModes: modePtr,
		}
		if req.Gateway != "" {
			row.Gateway = &req.Gateway
		}
		if req.EgressIf != "" {
			row.EgressIf = &req.EgressIf
		}
		if req.Remark != "" {
			row.Remark = &req.Remark
		}
		// 保留网段信息：地址属于哪个段是排查路由问题的第一条线索。
		if cidr := cidrOf(req.IP); cidr != "" {
			row.CIDR = &cidr
		}
		created = append(created, row)
	}

	if err := s.db.WithContext(ctx).Create(&created).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("地址已在池中（同一节点内地址唯一）")
		}
		log.Printf("[publicip] 写入地址失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "public_ip",
		ResourceName: req.IP, Action: "public_ip.create",
		Params:  map[string]any{"count": len(created), "input": req.IP},
		Success: true, ClientIP: clientIP,
	})

	return s.List(ctx, req.NodeID)
}

// BindRequest 是绑定请求。
type BindRequest struct {
	PublicIPID int64
	// VMID 为目标虚拟机。
	VMID int64
	Mode string
}

// Bind 把公网地址绑定到虚拟机（F-4-06）。
func (s *Service) Bind(
	ctx context.Context, req BindRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	ip, vm, err := s.loadForBind(ctx, req.PublicIPID, req.VMID, req.Mode, v)
	if err != nil {
		return nil, err
	}

	// 已有有效绑定时拒绝，而不是静默改绑。
	//
	// 「静默改绑」看起来更省事，但它让「这个地址原来指向谁」这个信息
	// 在一次点击里消失了——而如果用户是点错了一行，他连错在哪里都看不到。
	// 要换目标请用浮动迁移（Migrate），那个入口会明确说明在做什么。
	if existing, err := s.activeBinding(ctx, ip.ID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, api.Conflict("该地址已绑定，如需换绑请使用「浮动迁移」")
	}

	// 数量配额：公网地址是最稀缺的一种资源，没有上限时"先到先得"会把
	// 后来的人彻底挡在门外，而这通常不是管理员想要的结果。
	if s.computeQuota != nil && vm.OwnerID != nil {
		if err := s.computeQuota.Check(ctx, *vm.OwnerID, vm.NodeID, computequota.Additions{
			PublicIPs: 1,
		}); err != nil {
			return nil, err
		}
	}

	return s.dispatch(ctx, ip, vm, model.PublicIPModeLabel(req.Mode), req.Mode, "bind", v, operatorName, clientIP)
}

// MigrateRequest 是浮动迁移请求。
type MigrateRequest struct {
	PublicIPID int64
	// ToVMID 是新的目标虚拟机。
	ToVMID int64
	// Mode 留空时沿用当前绑定的模式。
	Mode string
}

// Migrate 把公网地址从当前虚拟机迁到另一台（F-4-06：浮动 IP 迁移）。
//
// 这是公网 IP 最有价值的场景——**故障转移**：主机器出问题时把地址挪到
// 备机上，外部访问几乎无感。也正因如此它必须原子：解绑与绑定之间如果
// 插进了失败，地址会**悬空**（不指向任何地方），而那时外部访问已经断了，
// 用户却以为迁移还在进行。
//
// 因此这里先校验全部前置条件，再入队；节点侧按「先撤旧、再加新」的顺序
// 一次做完，失败时旧绑定仍然有效。
func (s *Service) Migrate(
	ctx context.Context, req MigrateRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	ip, err := s.loadIP(ctx, req.PublicIPID)
	if err != nil {
		return nil, err
	}
	if ip.Status == model.PublicIPDisabled {
		return nil, api.ValidationFailed("该地址已停用，无法迁移")
	}

	existing, err := s.activeBinding(ctx, ip.ID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, api.ValidationFailed("该地址当前未绑定，请直接使用「绑定」")
	}
	if existing.VMID == nil {
		return nil, api.ValidationFailed("该地址当前仅被预留、未指向虚拟机，请直接使用「绑定」")
	}
	if *existing.VMID == req.ToVMID {
		return nil, api.ValidationFailed("目标虚拟机就是当前持有者，无需迁移")
	}

	mode := req.Mode
	if mode == "" {
		// 沿用当前模式：用户点「迁移」时想的是「把地址挪过去」，
		// 而不是「顺便换个工作方式」。要换模式他会明确选。
		mode = existing.Mode
	}

	vm, err := s.loadVM(ctx, req.ToVMID, ip.NodeID, v)
	if err != nil {
		return nil, err
	}
	if !ip.SupportsMode(mode) {
		return nil, api.ValidationFailed(
			"该地址不支持「" + model.PublicIPModeLabel(mode) + "」模式")
	}

	return s.dispatch(ctx, ip, vm, model.PublicIPModeLabel(mode), mode, "migrate", v, operatorName, clientIP)
}

// Unbind 解除地址的绑定。
//
// **不需要二次验证**：解绑不会造成不可逆的结果——地址还在池里，
// 随时可以再绑回去。它影响的是连通性，而连通性中断是**立刻可见**的，
// 用户马上就知道发生了什么。
func (s *Service) Unbind(
	ctx context.Context, publicIPID int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	ip, err := s.loadIP(ctx, publicIPID)
	if err != nil {
		return nil, err
	}
	existing, err := s.activeBinding(ctx, ip.ID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, api.ValidationFailed("该地址当前未绑定")
	}
	if err := s.ensureNode(ctx, ip.NodeID); err != nil {
		return nil, err
	}

	return s.dispatch(ctx, ip, nil, "", existing.Mode, "unbind", v, operatorName, clientIP)
}

// Preview 返回绑定/迁移将要下发的规则（F-4-06：规则预览）。
//
// **不产生任何改动**，也不入队：它是只读探测，与指标、截帧同类。
func (s *Service) Preview(
	ctx context.Context, publicIPID, vmID int64, mode string, v authz.Viewer,
) (*agent.PublicIPPreview, error) {
	ip, err := s.loadIP(ctx, publicIPID)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = model.PublicIPModeNAT
	}
	if !ip.SupportsMode(mode) {
		return nil, api.ValidationFailed(
			"该地址不支持「" + model.PublicIPModeLabel(mode) + "」模式")
	}

	op := agent.Operation{
		Kind:   agent.OpPublicIPPreview,
		NodeID: ip.NodeID,
		Target: ip.IP,
		Params: map[string]any{"mode": mode},
	}
	if vmID > 0 {
		vm, err := s.loadVM(ctx, vmID, ip.NodeID, v)
		if err != nil {
			return nil, err
		}
		op.Params["vm_name"] = vm.Name
	}

	result, err := s.agent.Execute(ctx, op)
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法生成规则预览")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	preview, ok := result.Data[agent.PublicIPPreviewKey].(agent.PublicIPPreview)
	if !ok {
		return nil, api.Unavailable("节点未返回规则预览")
	}
	return &preview, nil
}

// Delete 从池中移除一个地址。
//
// **已绑定的地址不允许移除**：先解绑。移除一个正在被使用的地址会让它的
// 绑定记录指向一条不存在的地址，而节点上那条规则仍然生效——控制面与
// 宿主机就此分叉，且没有任何一处在报错。
func (s *Service) Delete(
	ctx context.Context, publicIPID int64, v authz.Viewer, operatorName, clientIP string,
) error {
	ip, err := s.loadIP(ctx, publicIPID)
	if err != nil {
		return err
	}
	existing, err := s.activeBinding(ctx, ip.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		return api.Conflict("该地址已绑定，请先解绑再移除")
	}

	if err := s.db.WithContext(ctx).Delete(&model.PublicIP{}, ip.ID).Error; err != nil {
		log.Printf("[publicip] 移除地址失败: %v", err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: ip.NodeID, ResourceType: "public_ip", ResourceID: ip.ID,
		ResourceName: ip.IP, Action: "public_ip.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// dispatch 组织一次绑定/解绑/迁移任务。
func (s *Service) dispatch(
	ctx context.Context, ip *model.PublicIP, vm *model.VM,
	modeLabel, mode, action string, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	params := map[string]any{
		"public_ip_id": ip.ID,
		"ip":           ip.IP,
		"action":       action,
		"mode":         mode,
	}
	resourceName := ip.IP
	if vm != nil {
		params["vm_id"] = vm.ID
		params["vm_name"] = vm.Name
		resourceName = ip.IP + " → " + vm.Name
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskPublicIPChange,
		NodeID:       ip.NodeID,
		ResourceType: "public_ip",
		ResourceID:   ip.ID,
		ResourceName: resourceName,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: ip.NodeID, ResourceType: "public_ip", ResourceID: ip.ID,
		ResourceName: resourceName, Action: "public_ip." + action,
		Params: map[string]any{
			"task_id": t.ID, "mode": mode, "mode_label": modeLabel,
			"vm_id": vmIDOf(vm),
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// loadForBind 取出并校验绑定所需的地址与虚拟机。
func (s *Service) loadForBind(
	ctx context.Context, publicIPID, vmID int64, mode string, v authz.Viewer,
) (*model.PublicIP, *model.VM, error) {
	ip, err := s.loadIP(ctx, publicIPID)
	if err != nil {
		return nil, nil, err
	}
	if ip.Status == model.PublicIPDisabled {
		return nil, nil, api.ValidationFailed("该地址已停用，无法绑定")
	}
	if mode == "" {
		mode = model.PublicIPModeNAT
	}
	if !validMode(mode) {
		return nil, nil, api.InvalidParameter("不支持的绑定模式：" + mode)
	}
	// 地址自己声明支持哪些模式（f-4-06）：让用户去试错会得到一句来自
	// 内核的报错，而不是「这个地址不支持这种模式」。
	if !ip.SupportsMode(mode) {
		return nil, nil, api.ValidationFailed(
			"该地址不支持「" + model.PublicIPModeLabel(mode) + "」模式")
	}
	if err := s.ensureNode(ctx, ip.NodeID); err != nil {
		return nil, nil, err
	}

	vm, err := s.loadVM(ctx, vmID, ip.NodeID, v)
	if err != nil {
		return nil, nil, err
	}
	return ip, vm, nil
}

// loadIP 读取地址。
func (s *Service) loadIP(ctx context.Context, id int64) (*model.PublicIP, error) {
	var ip model.PublicIP
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&ip).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("公网地址不存在")
	case err != nil:
		log.Printf("[publicip] 查询地址失败: %v", err)
		return nil, api.Internal()
	}
	return &ip, nil
}

// loadVM 读取并校验目标虚拟机：必须存在、属于同一节点、且调用者有权操作。
func (s *Service) loadVM(
	ctx context.Context, vmID, nodeID int64, v authz.Viewer,
) (*model.VM, error) {
	var vm model.VM
	err := s.db.WithContext(ctx).Where("id = ?", vmID).First(&vm).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("虚拟机不存在")
	case err != nil:
		log.Printf("[publicip] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}

	// 用 404 而非 403：403 会确认「这个 ID 存在」，让 tenant 能通过枚举
	// 推断出别人有多少虚拟机。
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return nil, api.NotFound("虚拟机不存在")
	}
	// 地址与虚拟机必须在同一台宿主机上：公网地址是节点内资源，指向另一台
	// 机器上的虚拟机没有任何网络路径可走。
	if vm.NodeID != nodeID {
		return nil, api.ValidationFailed(
			"该虚拟机在另一台宿主机上；公网地址只能绑定同一节点内的虚拟机")
	}
	return &vm, nil
}

// activeBinding 返回地址当前有效的绑定；没有则返回 nil。
func (s *Service) activeBinding(ctx context.Context, publicIPID int64) (*model.PublicIPBinding, error) {
	var b model.PublicIPBinding
	err := s.db.WithContext(ctx).
		Where("public_ip_id = ? AND released_at IS NULL", publicIPID).
		First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		log.Printf("[publicip] 查询绑定失败: %v", err)
		return nil, api.Internal()
	}
	return &b, nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	err := s.db.WithContext(ctx).
		Select("id", "maintenance_mode", "enroll_state").
		Where("id = ?", nodeID).First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[publicip] 查询节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("节点尚未接入，无法管理公网地址")
	}
	// 维护模式拦在**受理**处：维护的意义就是不引入变更，而公网地址的
	// 绑定正是网络层面的一次变更。
	if n.MaintenanceMode {
		return api.ValidationFailed("节点处于维护模式，已暂停网络变更")
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func toView(
	ip *model.PublicIP, b *model.PublicIPBinding, vmNames map[int64]string,
) View {
	view := View{
		ID: ip.ID, NodeID: ip.NodeID, IP: ip.IP,
		AddressFamily:  ip.AddressFamily,
		SupportedModes: ip.Modes(),
		Status:         ip.Status,
	}
	if ip.CIDR != nil {
		view.CIDR = *ip.CIDR
	}
	if ip.Gateway != nil {
		view.Gateway = *ip.Gateway
	}
	if ip.EgressIf != nil {
		view.EgressIf = *ip.EgressIf
	}
	if ip.Remark != nil {
		view.Remark = *ip.Remark
	}
	if b != nil {
		id := b.ID
		view.BindingID = &id
		view.VMID = b.VMID
		view.Mode = b.Mode
		view.RuntimeStatus = b.RuntimeStatus
		if b.VMID != nil {
			view.VMName = vmNames[*b.VMID]
		}
		if b.BoundAt != nil {
			view.BoundAt = b.BoundAt.Format("2006-01-02T15:04:05Z07:00")
		}
	}
	return view
}

// expand 把单地址或 CIDR 展开成一组可分配的地址。
func expand(input, gateway string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, api.InvalidParameter("必须填写 IP 地址或网段")
	}

	if !strings.Contains(input, "/") {
		if net.ParseIP(input) == nil {
			return nil, api.InvalidParameter("IP 地址格式不正确：" + input)
		}
		return []string{input}, nil
	}

	ip, ipnet, err := net.ParseCIDR(input)
	if err != nil {
		return nil, api.InvalidParameter("网段格式不正确，应形如 203.0.113.0/24")
	}
	// 给一个上限：一个 /8 会展开出一千六百多万条记录。这不是性能洁癖——
	// 用户多半是打错了一位掩码，而照做会让库里多出一批他完全没打算录入
	// 的地址，且清理起来同样困难。
	if ones, bits := ipnet.Mask.Size(); bits-ones > 12 {
		return nil, api.InvalidParameter(fmt.Sprintf(
			"网段过大（/%d），一次最多录入 /%d（即 %d 个地址）；"+
				"请分批录入或检查掩码是否写错", ones, bits-12, 4096))
	}

	var out []string
	// 从网络地址开始逐一遍历，跳过网络地址、广播地址与网关。
	for cur := ip.Mask(ipnet.Mask); ipnet.Contains(cur); cur = nextIP(cur) {
		s := cur.String()
		// 网络地址与广播地址不能被分配给虚拟机（IPv4）。
		if isIPv4(s) && (s == ipnet.IP.String() || isBroadcast(cur, ipnet)) {
			continue
		}
		if gateway != "" && s == gateway {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, api.InvalidParameter("该网段没有可分配的地址")
	}
	return out, nil
}

func nextIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

func isBroadcast(ip net.IP, ipnet *net.IPNet) bool {
	if !isIPv4(ip.String()) {
		return false
	}
	mask := ipnet.Mask
	if len(mask) != 4 {
		return false
	}
	// 广播地址 = 网络地址 | ~掩码
	out := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		out[i] = ip.To4()[i] | ^mask[i]
	}
	return ip.Equal(out)
}

func familyOf(ip string) string {
	if isIPv6(ip) {
		return "ipv6"
	}
	return "ipv4"
}

func isIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() != nil
}

func isIPv6(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() == nil
}

func cidrOf(input string) string {
	if !strings.Contains(input, "/") {
		return ""
	}
	_, ipnet, err := net.ParseCIDR(input)
	if err != nil {
		return ""
	}
	return ipnet.String()
}

func validMode(mode string) bool {
	switch mode {
	case model.PublicIPModeNAT, model.PublicIPModeRouted, model.PublicIPModeBridged:
		return true
	}
	return false
}

func vmIDOf(vm *model.VM) any {
	if vm == nil {
		return nil
	}
	return vm.ID
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
