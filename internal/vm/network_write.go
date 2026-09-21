package vm

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// 网络变更的动作。
const (
	NetActionAdd    = "add"
	NetActionUpdate = "update"
	NetActionRemove = "remove"
	NetActionBind   = "bind"
	NetActionUnbind = "unbind"
)

// defaultInterfaceLimit 是单台虚拟机的网卡上限。
//
// 与快照配额一样先放一个常量而不是设置项：最终应来自模板，而模板尚未实现。
// 立刻需要一个上限是因为网卡序号（order）是唯一的，无限制地加会让界面上的
// 「第几块网卡」失去意义。
const defaultInterfaceLimit = 8

// --- 网卡 ---

// InterfaceRequest 是新增或修改网卡的请求。
type InterfaceRequest struct {
	// Model 取值 virtio / e1000 / rtl8139。
	Model string
	// SwitchID 为空表示使用节点默认网络。
	SwitchID *int64
	// RateLimitMbps 为 0 表示不限速。
	RateLimitMbps int
	// AllowedAddresses 是允许的源地址（防 IP 欺骗），逗号分隔。
	AllowedAddresses string
}

// AddInterface 受理一次新增网卡。
//
// 网卡**不需要关机**（虚拟化层支持热插拔），因此这里不探测运行态——
// 让用户为了加一块网卡去停机是不必要的。
func (s *Service) AddInterface(
	ctx context.Context, vmID int64, req InterfaceRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	if err := validateNICModel(req.Model); err != nil {
		return nil, err
	}
	if err := validateRateLimit(req.RateLimitMbps); err != nil {
		return nil, err
	}
	if err := s.checkBandwidthQuota(ctx, vm, req.RateLimitMbps, 0); err != nil {
		return nil, err
	}

	var existing []model.VMInterface
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).Find(&existing).Error; err != nil {
		log.Printf("[vm] 查询网卡失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}
	if len(existing) >= defaultInterfaceLimit {
		return nil, api.Conflict(
			"网卡数量已达上限（" + strconv.Itoa(defaultInterfaceLimit) + "）")
	}

	// 新网卡的序号取「当前最大 + 1」而不是「数量」：删掉中间的某一块之后，
	// 用数量当序号会与已有的冲突，而冲突的表现是唯一约束报错——用户看到的是
	// 「添加失败」却不知道原因。
	next := 0
	for _, nic := range existing {
		if nic.Order >= next {
			next = nic.Order + 1
		}
	}

	// MAC 由控制面生成并固定下来。交给节点随机分配的话，同一块网卡在每次
	// 重建后会得到不同的 MAC，而来宾里可能已经按 MAC 配好了网络。
	mac := MACFor(vm.ID, next)

	nic := model.VMInterface{
		VMID: vm.ID, NodeID: vm.NodeID, Order: next,
		Model: req.Model, MAC: &mac,
		SwitchID:         req.SwitchID,
		RateLimitMbps:    req.RateLimitMbps,
		AllowedAddresses: optStr(strings.TrimSpace(req.AllowedAddresses)),
		// IsPrimary 为 false：新增的永远是次要网卡。主网卡在创建虚拟机时
		// 确定，改它属于另一个操作（且会影响重装系统的恢复路径）。
		IsPrimary: false,
	}

	// 先写记录：执行器需要网卡 ID 才能把下发结果写回同一条记录。
	if err := s.db.WithContext(ctx).Create(&nic).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该网卡序号已被占用，请刷新后重试")
		}
		log.Printf("[vm] 创建网卡记录失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMInterfaceChange, "vm.interface.add.request", NicSpec(nic))
}

// UpdateInterface 受理一次网卡修改。
//
// 只改**可在运行中调整**的属性（型号、限速、允许的源地址、接入的交换机）。
// 网卡序号（order）不可改：它同时是来宾系统里的设备顺序，改了会让 eth0/eth1
// 对调，而来宾里可能已经按原来的顺序配好了路由。
func (s *Service) UpdateInterface(
	ctx context.Context, vmID, nicID int64, req InterfaceRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 先确认这块网卡确实属于这台虚拟机。少了这一步，任何知道网卡 ID 的人
	// 都能改一个自己无权访问的资源——归属校验不能只做在虚拟机上。
	if _, err := s.loadInterface(ctx, vmID, nicID); err != nil {
		return nil, err
	}

	if err := validateNICModel(req.Model); err != nil {
		return nil, err
	}
	if err := validateRateLimit(req.RateLimitMbps); err != nil {
		return nil, err
	}
	// 排除这块网卡自己：否则"把 100 改成 120"会算出 100 + 120 而永远超限。
	if err := s.checkBandwidthQuota(ctx, vm, req.RateLimitMbps, nicID); err != nil {
		return nil, err
	}

	updates := map[string]any{
		"model":             req.Model,
		"switch_id":         req.SwitchID,
		"rate_limit_mbps":   req.RateLimitMbps,
		"allowed_addresses": optStr(strings.TrimSpace(req.AllowedAddresses)),
		"last_applied_at":   nil, // 尚未下发，界面据此显示「未生效」
		"updated_at":        time.Now(),
	}
	if err := s.db.WithContext(ctx).Model(&model.VMInterface{}).
		Where("id = ? AND vm_id = ?", nicID, vmID).Updates(updates).Error; err != nil {
		log.Printf("[vm] 更新网卡失败 id=%d: %v", nicID, err)
		return nil, api.Internal()
	}

	// 重新读出来构造指令：下发的是**完整的目标状态**，而不是增量补丁。
	// 节点侧不需要知道「之前是什么」，它只负责把当前配置对齐到控制面描述
	// 的样子——这让重试变成幂等的。
	updated, err := s.loadInterface(ctx, vmID, nicID)
	if err != nil {
		return nil, err
	}
	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMInterfaceChange, "vm.interface.update.request", NicSpec(*updated))
}

// RemoveInterface 受理一次网卡删除。
func (s *Service) RemoveInterface(
	ctx context.Context, vmID, nicID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	nic, err := s.loadInterface(ctx, vmID, nicID)
	if err != nil {
		return nil, err
	}

	// 主网卡不可删除：重装系统（f-2-11）依赖它保持网络可达，
	// 删掉它就没有恢复路径了。
	if nic.IsPrimary {
		return nil, api.ValidationFailed("主网卡不能删除：重装系统等操作依赖它保持网络可达")
	}

	// 先删记录再入队：删除是幂等的，节点侧找不到这块网卡也应视为成功。
	// 反过来（先入队后删）会让执行器在一个已经消失的记录上写结果。
	if err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", nicID, vmID).
		Delete(&model.VMInterface{}).Error; err != nil {
		log.Printf("[vm] 删除网卡失败 id=%d: %v", nicID, err)
		return nil, api.Internal()
	}

	spec := NicSpec(*nic)
	spec["action"] = NetActionRemove
	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMInterfaceChange, "vm.interface.remove.request", spec)
}

// NicSpec 构造网卡的下发描述。
//
// 导出供执行器与测试使用：下发的是**完整的目标状态**而非增量，
// 因此这份描述在「新增」与「修改」两种动作下是同一个形状。
func NicSpec(nic model.VMInterface) map[string]any {
	spec := map[string]any{
		"action":          NetActionUpdate,
		"interface_id":    nic.ID,
		"order":           nic.Order,
		"is_primary":      nic.IsPrimary,
		"model":           nic.Model,
		"rate_limit_mbps": nic.RateLimitMbps,
	}
	if nic.MAC != nil {
		spec["mac"] = *nic.MAC
	}
	if nic.SwitchID != nil {
		spec["switch_id"] = *nic.SwitchID
	}
	if nic.AllowedAddresses != nil {
		spec["allowed_addresses"] = *nic.AllowedAddresses
	}
	return spec
}

// loadInterface 读取属于该虚拟机的网卡。
func (s *Service) loadInterface(
	ctx context.Context, vmID, nicID int64,
) (*model.VMInterface, error) {
	var nic model.VMInterface
	err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", nicID, vmID).
		First(&nic).Error
	if err != nil {
		if isNotFound(err) {
			return nil, api.NotFound("网卡不存在")
		}
		log.Printf("[vm] 查询网卡失败 id=%d: %v", nicID, err)
		return nil, api.Internal()
	}
	return &nic, nil
}

// validateNICModel 校验网卡型号。
//
// 取值来自 model 包的常量而不是本文件里的字面量：型号同时出现在迁移、模型与
// 界面三处，各写一份必然漂移。
func validateNICModel(kind string) error {
	switch kind {
	case model.NICModelVirtio, model.NICModelE1000, model.NICModelRTL8139:
		return nil
	default:
		return api.InvalidParameter("不支持的网卡型号，可选 virtio / e1000 / rtl8139")
	}
}

func validateRateLimit(mbps int) error {
	// 0 表示不限速；上限 100000 Mbps（100 Gbps）足以覆盖任何单机场景，
	// 同时挡住手滑多打了几个零的值——那会让这块网卡实际上不可用。
	if mbps < 0 || mbps > 100000 {
		return api.InvalidParameter("限速值应在 0 到 100000 Mbps 之间（0 表示不限速）")
	}
	return nil
}

// MACFor 生成一个本地的单播 MAC 地址。
//
// 前缀 52:54:00 是 QEMU/KVM 的保留段；用固定前缀而不是随机前缀，是为了让
// 「这台虚拟机的网卡」在抓包与交换机的表项里一眼可辨。
//
// 末三位由虚拟机 ID 与序号推导而不是随机：**同一块网卡在重建后应当拿到
// 同一个 MAC**，否则来宾里按 MAC 做的配置（静态网络、udev 规则）会失效。
//
// 导出供测试验证这条不变量——它是「网卡删了再加，来宾仍然能上网」的前提。
func MACFor(vmID int64, order int) string {
	a := byte((vmID >> 8) & 0xff)
	b := byte(vmID & 0xff)
	c := byte(order & 0xff)
	return "52:54:00:" + hex2(a) + ":" + hex2(b) + ":" + hex2(c)
}

func hex2(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}

// --- 静态地址 ---

// BindStaticIPRequest 是绑定静态地址的请求。
type BindStaticIPRequest struct {
	IP string
	// InterfaceOrder 指定绑到哪块网卡。
	InterfaceOrder *int
	// IsDHCPReservation 表示这条地址通过 DHCP 静态租约下发（而不是让用户
	// 在来宾里手工配置）。
	IsDHCPReservation bool
}

// BindStaticIP 受理一次静态地址绑定。
func (s *Service) BindStaticIP(
	ctx context.Context, vmID int64, req BindStaticIPRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	ip := strings.TrimSpace(req.IP)
	if ip == "" {
		return nil, api.InvalidParameter("请填写要绑定的地址")
	}
	if err := validateIP(ip); err != nil {
		return nil, err
	}

	// 地址在**节点内**唯一：同一个地址分配给两台虚拟机，只有一台能真正
	// 用上它，而另一台会表现为「网络时通时断」——这类问题极难排查。
	var count int64
	if err := s.db.WithContext(ctx).Model(&model.StaticIP{}).
		Where("node_id = ? AND ip = ? AND vm_id IS NOT NULL AND vm_id <> ?",
			vm.NodeID, ip, vm.ID).
		Count(&count).Error; err != nil {
		log.Printf("[vm] 检查地址占用失败: %v", err)
		return nil, api.Internal()
	}
	if count > 0 {
		return nil, api.Conflict("该地址已被同一节点上的其它虚拟机占用")
	}

	row := model.StaticIP{
		NodeID:            vm.NodeID,
		VMID:              &vm.ID,
		IP:                ip,
		AddressFamily:     addressFamilyOf(ip),
		InterfaceOrder:    req.InterfaceOrder,
		IsDHCPReservation: req.IsDHCPReservation,
	}

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该地址已存在")
		}
		log.Printf("[vm] 创建静态地址失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMStaticIPChange, "vm.staticip.bind.request", map[string]any{
			"action":              NetActionBind,
			"static_ip_id":        row.ID,
			"ip":                  row.IP,
			"address_family":      row.AddressFamily,
			"interface_order":     req.InterfaceOrder,
			"is_dhcp_reservation": row.IsDHCPReservation,
		})
}

// UnbindStaticIP 受理一次静态地址解绑。
func (s *Service) UnbindStaticIP(
	ctx context.Context, vmID, staticIPID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	var row model.StaticIP
	err = s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", staticIPID, vmID).
		First(&row).Error
	if err != nil {
		if isNotFound(err) {
			return nil, api.NotFound("静态地址不存在")
		}
		log.Printf("[vm] 查询静态地址失败 id=%d: %v", staticIPID, err)
		return nil, api.Internal()
	}

	spec := map[string]any{
		"action":              NetActionUnbind,
		"static_ip_id":        row.ID,
		"ip":                  row.IP,
		"interface_order":     row.InterfaceOrder,
		"is_dhcp_reservation": row.IsDHCPReservation,
	}

	// 先删记录再入队，与网卡删除同理：解绑是幂等的。
	if err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", staticIPID, vmID).
		Delete(&model.StaticIP{}).Error; err != nil {
		log.Printf("[vm] 删除静态地址失败 id=%d: %v", staticIPID, err)
		return nil, api.Internal()
	}

	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMStaticIPChange, "vm.staticip.unbind.request", spec)
}

// --- 端口转发 ---

// PortForwardView 是端口转发在接口层的形态。
type PortForwardView struct {
	ID       int64  `json:"id"`
	Protocol string `json:"protocol"`
	HostPort int    `json:"host_port"`
	// TargetIP 为空表示由节点按虚拟机的实际地址决定。
	TargetIP   *string `json:"target_ip,omitempty"`
	TargetPort int     `json:"target_port"`
	// AllowedIPs 为空表示**不限制来源**。界面必须显式提示这一点。
	AllowedIPs *string `json:"allowed_ips,omitempty"`
	// AllowedRegions 当前尚未生效（需要节点侧 GeoIP），界面应如实标注。
	AllowedRegions *string `json:"allowed_regions,omitempty"`
	Enabled        bool    `json:"enabled"`
	// Applied 为 false 表示规则尚未下发，即**当前并不生效**。
	Applied       bool       `json:"applied"`
	LastAppliedAt *time.Time `json:"last_applied_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// PortForwards 返回虚拟机的端口转发列表。
func (s *Service) PortForwards(
	ctx context.Context, vmID int64, v authz.Viewer,
) ([]PortForwardView, error) {
	if _, err := s.load(ctx, vmID, v); err != nil {
		return nil, err
	}

	var rows []model.PortForward
	err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).
		Order("host_port").
		Find(&rows).Error
	if err != nil {
		log.Printf("[vm] 查询端口转发失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	out := make([]PortForwardView, 0, len(rows))
	for _, r := range rows {
		out = append(out, PortForwardView{
			ID: r.ID, Protocol: r.Protocol, HostPort: r.HostPort,
			TargetIP: r.TargetIP, TargetPort: r.TargetPort,
			AllowedIPs: r.AllowedIPs, AllowedRegions: r.AllowedRegions,
			Enabled:       r.Enabled,
			Applied:       r.LastAppliedAt != nil,
			LastAppliedAt: r.LastAppliedAt,
			CreatedAt:     r.CreatedAt,
		})
	}
	return out, nil
}

// AddPortForwardRequest 是新增端口转发的请求。
type AddPortForwardRequest struct {
	Protocol   string
	HostPort   int
	TargetIP   string
	TargetPort int
	// AllowedIPs 为空表示不限制来源。
	AllowedIPs string
}

// AddPortForward 受理一次端口转发新增。
//
// 端口转发是**把内网服务暴露到外部**的操作，因此提示文案与默认值都要偏向
// 保守：允许来源默认为空（不限制）时界面必须明确警告，而不是让用户以为
// 它已经受限。
func (s *Service) AddPortForward(
	ctx context.Context, vmID int64, req AddPortForwardRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	protocol := strings.ToLower(strings.TrimSpace(req.Protocol))
	if protocol != model.PortProtocolTCP && protocol != model.PortProtocolUDP {
		return nil, api.InvalidParameter("协议只能是 tcp 或 udp")
	}
	if err := validatePort(req.HostPort, "宿主机端口"); err != nil {
		return nil, err
	}
	if err := validatePort(req.TargetPort, "目标端口"); err != nil {
		return nil, err
	}

	// 端口在**节点内独占**（唯一索引也是这么建的）。两个转发抢同一个端口
	// 只会让其中一个静默失效，而用户会以为两条规则都在工作。
	var count int64
	if err := s.db.WithContext(ctx).Model(&model.PortForward{}).
		Where("node_id = ? AND protocol = ? AND host_port = ?",
			vm.NodeID, protocol, req.HostPort).
		Count(&count).Error; err != nil {
		log.Printf("[vm] 检查端口占用失败: %v", err)
		return nil, api.Internal()
	}
	if count > 0 {
		return nil, api.Conflict(
			"该节点上的 " + protocol + " 端口 " + strconv.Itoa(req.HostPort) + " 已被其它转发占用")
	}

	// 数量配额按**用户 × 节点**计，而不是按这台机器：端口转发消耗的是
	// 宿主机的端口，用户换一台机器来建并不会让消耗变少。
	if s.computeQuota != nil && vm.OwnerID != nil {
		if err := s.computeQuota.Check(ctx, *vm.OwnerID, vm.NodeID, computequota.Additions{
			PortForwards: 1,
		}); err != nil {
			return nil, err
		}
	}

	row := model.PortForward{
		NodeID: vm.NodeID, VMID: &vm.ID,
		Protocol: protocol, HostPort: req.HostPort,
		TargetIP:   optStr(strings.TrimSpace(req.TargetIP)),
		TargetPort: req.TargetPort,
		AllowedIPs: optStr(strings.TrimSpace(req.AllowedIPs)),
		Enabled:    true,
		CreatedBy:  &v.UserID,
	}

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该端口已被占用")
		}
		log.Printf("[vm] 创建端口转发失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMPortForwardChange, "vm.portforward.add.request", portForwardSpec(row, NetActionAdd))
}

// RemovePortForward 受理一次端口转发删除。
func (s *Service) RemovePortForward(
	ctx context.Context, vmID, pfID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	var row model.PortForward
	err = s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", pfID, vmID).
		First(&row).Error
	if err != nil {
		if isNotFound(err) {
			return nil, api.NotFound("端口转发不存在")
		}
		log.Printf("[vm] 查询端口转发失败 id=%d: %v", pfID, err)
		return nil, api.Internal()
	}

	spec := portForwardSpec(row, NetActionRemove)

	if err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", pfID, vmID).
		Delete(&model.PortForward{}).Error; err != nil {
		log.Printf("[vm] 删除端口转发失败 id=%d: %v", pfID, err)
		return nil, api.Internal()
	}

	return s.enqueueNetChange(ctx, vm, v, operatorName, clientIP,
		model.TaskVMPortForwardChange, "vm.portforward.remove.request", spec)
}

// portForwardSpec 构造端口转发的下发描述。
func portForwardSpec(row model.PortForward, action string) map[string]any {
	spec := map[string]any{
		"action":      action,
		"protocol":    row.Protocol,
		"host_port":   row.HostPort,
		"target_port": row.TargetPort,
		"enabled":     row.Enabled,
	}
	if row.TargetIP != nil {
		spec["target_ip"] = *row.TargetIP
	}
	if row.AllowedIPs != nil {
		spec["allowed_ips"] = *row.AllowedIPs
	}
	return spec
}

// --- 公共 ---

// enqueueNetChange 把一次网络变更入队。
//
// 三种网络资源共用这一个入口，因为它们的受理流程完全一致：归属校验
// （调用方已做）→ 写好本地记录 → 入队 → 写审计。资源锁键统一用 vm:<id>。
func (s *Service) enqueueNetChange(
	ctx context.Context, vm *model.VM, v authz.Viewer,
	operatorName, clientIP, taskType, auditAction string, params map[string]any,
) (*model.Task, error) {
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         taskType,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:     auditAction,
		Params:     params,
		AfterState: map[string]any{"task_id": t.ID},
		Success:    true, ClientIP: clientIP,
	})
	return t, nil
}

// validatePort 校验端口号。
func validatePort(port int, label string) error {
	if port < 1 || port > 65535 {
		return api.InvalidParameter(label + "应在 1 到 65535 之间")
	}
	// 1024 以下的端口需要特权，且很可能是宿主机上别的服务在监听。
	// 这里不禁止（用户可能有正当理由），但界面上应给出提示。
	return nil
}

// validateIP 校验地址格式。
//
// 只做最基本的形态检查，不解析为具体地址类型：控件的 type=text 与后端
// 的正则应该是同一套宽严标准，过严会让用户无法输入合法的 IPv6 地址。
func validateIP(ip string) error {
	if !strings.Contains(ip, ":") && !strings.Contains(ip, ".") {
		return api.InvalidParameter("地址格式不正确")
	}
	if strings.ContainsAny(ip, " \t\n") {
		return api.InvalidParameter("地址中不能包含空格")
	}
	return nil
}

// addressFamilyOf 判断地址族。
func addressFamilyOf(ip string) string {
	if strings.Contains(ip, ":") {
		return model.AddressFamilyIPv6
	}
	return model.AddressFamilyIPv4
}

// isNotFound 判断错误是否为「记录不存在」。
//
// 用 errors.Is 而不是比较错误字符串：字符串比较在驱动或语言环境变化时会
// 静默失效，而失效的表现是「查不到记录」被当成内部错误返回 500——
// 一个本该 404 的场景变成 500，排查时会先去怀疑数据库。
func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}
