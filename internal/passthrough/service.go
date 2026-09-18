// Package passthrough 实现 PCIe 设备直通（GPU 等）。
//
// 有一件事决定了本包大部分设计：**IOMMU 分组里的设备只能一起直通**。
//
// 它不是实现细节，而是用户最先撞上的那堵墙——他只想把显卡挂进虚拟机，
// 结果发现鼠标键盘也一起被拿走了（它们和显卡在同一个分组里）。而分组关系
// 取决于主板拓扑与 BIOS 设置，**只有节点探测得到**，控制面无从推断。
//
// 因此：探测结果必须带同组成员、界面上必须在用户选择的那一刻就把"同组的
// 另外三个设备会一起被拿走"说出来、而挂载时也必须校验同组没有冲突。
package passthrough

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Service 提供直通能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
	now   func() time.Time
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder, now: time.Now}
}

// DeviceView 是一块设备的对外视图。
type DeviceView struct {
	agent.PCIDevice
	// GroupPeers 是同组设备的描述（供界面直接展示"会一起被拿走"）。
	GroupPeers []string `json:"group_peers"`
	// AttachedToVM 是被哪台虚拟机挂着（控制面记录）。
	AttachedToVMID   int64  `json:"attached_to_vm_id,omitempty"`
	AttachedToVMName string `json:"attached_to_vm_name,omitempty"`
}

// Overview 返回设备清单与 IOMMU 状态。
func (s *Service) Overview(ctx context.Context, nodeID int64) ([]DeviceView, *agent.IOMMUStatus, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostPCIDevices, NodeID: nodeID,
	})
	if err != nil {
		return nil, nil, api.Unavailable("节点不可达，无法读取设备清单")
	}
	if !result.Success {
		return nil, nil, api.ValidationFailed(result.Message)
	}
	devices, _ := result.Data[agent.PCIDevicesKey].([]agent.PCIDevice)

	iommuResult, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostIOMMU, NodeID: nodeID,
	})
	var iommu *agent.IOMMUStatus
	if err == nil && iommuResult.Success {
		if st, ok := iommuResult.Data[agent.IOMMUStatusKey].(agent.IOMMUStatus); ok {
			iommu = &st
		}
	}

	// 控制面补上「被哪台虚拟机挂着」——节点不知道这件事，它只知道设备绑到
	// 了哪个驱动。而这个信息很重要：解绑一块正被虚拟机使用的卡会**拔掉那台
	// 运行中机器的"显卡"**。
	attached, err := s.attachedMap(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}
	names := s.vmNames(ctx, attached)

	// 同组索引：让界面能直接展示"会一起被拿走"。
	byGroup := map[int][]string{}
	for _, d := range devices {
		byGroup[d.IOMUGroup] = append(byGroup[d.IOMUGroup], d.Address)
	}

	out := make([]DeviceView, 0, len(devices))
	for _, d := range devices {
		v := DeviceView{PCIDevice: d, GroupPeers: []string{}}
		for _, addr := range byGroup[d.IOMUGroup] {
			if addr == d.Address {
				continue
			}
			v.GroupPeers = append(v.GroupPeers, describeOf(devices, addr))
		}
		sort.Strings(v.GroupPeers)
		if vmID, ok := attached[d.Address]; ok {
			v.AttachedToVMID = vmID
			v.AttachedToVMName = names[vmID]
		}
		out = append(out, v)
	}
	return out, iommu, nil
}

// Bind 把设备绑定到 vfio-pci。
func (s *Service) Bind(
	ctx context.Context, nodeID int64, address string,
	v authz.Viewer, operatorName, clientIP string,
) error {
	return s.bindOrUnbind(ctx, nodeID, address, true, v, operatorName, clientIP)
}

// Unbind 把设备解绑回宿主驱动。
func (s *Service) Unbind(
	ctx context.Context, nodeID int64, address string,
	v authz.Viewer, operatorName, clientIP string,
) error {
	return s.bindOrUnbind(ctx, nodeID, address, false, v, operatorName, clientIP)
}

func (s *Service) bindOrUnbind(
	ctx context.Context, nodeID int64, address string, bind bool,
	v authz.Viewer, operatorName, clientIP string,
) error {
	if strings.TrimSpace(address) == "" {
		return api.InvalidParameter("必须指定 PCI 地址")
	}

	// **解绑前必须确认没有虚拟机在用。**
	//
	// 解绑一块正被虚拟机使用的卡，等于在那台机器运行中把它的"显卡"拔掉——
	// 而用户点这个按钮时想的多半是"我不需要了"，不是"我要弄坏一台机器"。
	attached, err := s.attachedMap(ctx, nodeID)
	if err != nil {
		return err
	}
	if vmID, inUse := attached[address]; inUse {
		if !bind {
			return api.Conflict("该设备正被虚拟机使用，请先在虚拟机上卸载它")
		}
		// 重新绑定一个正在被使用的设备同样是危险动作：改驱动会让它
		// 在虚拟机里消失。
		return api.Conflict("该设备正被虚拟机使用（#" + itoa(vmID) +
			"）。请先卸载它——宿主驱动的改动会让它在虚拟机里消失")
	}

	action := "unbind"
	if bind {
		action = "bind"
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostPCIBind, NodeID: nodeID, Target: address,
		Params: map[string]any{"action": action, "address": address},
	})
	if err != nil {
		return api.Unavailable("节点不可达，设备未" + map[bool]string{true: "绑定", false: "解绑"}[bind])
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "pci_device",
		ResourceName: address, Action: "pci." + action,
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Attach 把设备挂到虚拟机上。
func (s *Service) Attach(
	ctx context.Context, vmID int64, address string, remark string,
	v authz.Viewer, operatorName, clientIP string,
) (*model.VMPassthrough, error) {
	if strings.TrimSpace(address) == "" {
		return nil, api.InvalidParameter("必须指定 PCI 地址")
	}
	vm, err := s.loadVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if vm.Status == model.VMStatusRunning {
		// 直通设备不支持热插拔（有限的支持需要内核与固件的配合，而失败
		// 方式是一台机器卡住在半启动状态）。**要求先关机**是刻意的：
		// 用户以为能热插而实际不能时，坏掉的是他那台正在跑的机器。
		return nil, api.Conflict("直通设备需要虚拟机处于关机状态才能挂载——它不支持热插拔")
	}

	devices, _, err := s.Overview(ctx, vm.NodeID)
	if err != nil {
		return nil, err
	}
	var target *DeviceView
	for i := range devices {
		if devices[i].Address == address {
			target = &devices[i]
			break
		}
	}
	if target == nil {
		return nil, api.NotFound("该节点上没有这个设备")
	}
	if !target.CanPassthrough {
		return nil, api.ValidationFailed("该设备不可直通：" + target.Reason)
	}
	if target.AttachedToVMID != 0 && target.AttachedToVMID != vmID {
		return nil, api.Conflict("该设备已被其它虚拟机挂载")
	}

	// **同组冲突检查。**
	//
	// IOMMU 分组里的设备只能一起直通，因此一个分组**只能属于一台虚拟机**。
	// 不检查的话，用户把同组的两块卡分别挂给两台机器，而 libvirt 只会在
	// 第二台机器启动时失败——那时他已经把第一台也跑起来了，两边都看不出
	// 问题出在"分组"上。
	if err := s.checkGroupConflict(ctx, vm.NodeID, vmID, target); err != nil {
		return nil, err
	}

	row := model.VMPassthrough{
		VMID: vmID, NodeID: vm.NodeID, PCIAddress: address,
		AttachedAt: s.now(),
	}
	desc := target.Description
	row.DeviceDesc = &desc
	if target.IOMUGroup > 0 {
		g := target.IOMUGroup
		row.IOMUGroup = &g
	}
	if remark != "" {
		row.Remark = &remark
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicate(err) {
			return nil, api.Conflict("该设备已经挂在这台虚拟机上了")
		}
		log.Printf("[passthrough] 创建失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskPassthroughChange,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vmID,
		ResourceName: vm.Name,
		OwnerID:      v.UserID, CreatedBy: v.UserID,
		Params: map[string]any{
			"action": "attach", "vm_id": vmID, "vm_name": vm.Name,
			"pci_address": address, "device_desc": desc,
		},
	})
	if err != nil {
		return nil, api.Internal()
	}
	_ = t

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vmID, ResourceName: vm.Name,
		Action: "vm.passthrough.attach",
		Params: map[string]any{
			"pci_address": address, "device_desc": desc,
			// 分组入审计：事后排查"为什么这台机器起不来"时，
			// "当时挂在哪个分组"是唯一能解释它的东西。
			"iommu_group": target.IOMUGroup,
		},
		Success: true, ClientIP: clientIP,
	})
	return &row, nil
}

// Detach 从虚拟机上卸载设备。
func (s *Service) Detach(
	ctx context.Context, vmID int64, address string,
	v authz.Viewer, operatorName, clientIP string,
) error {
	vm, err := s.loadVM(ctx, vmID)
	if err != nil {
		return err
	}
	if vm.Status == model.VMStatusRunning {
		return api.Conflict("请先关机再卸载直通设备——热拔会让虚拟机卡在半启动状态")
	}

	var row model.VMPassthrough
	if err := s.db.WithContext(ctx).
		Where("vm_id = ? AND pci_address = ?", vmID, address).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("该设备没有挂在这台虚拟机上")
		}
		return api.Internal()
	}
	if err := s.db.WithContext(ctx).Delete(&model.VMPassthrough{}, row.ID).Error; err != nil {
		return api.Internal()
	}

	if _, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskPassthroughChange,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vmID,
		ResourceName: vm.Name,
		OwnerID:      v.UserID, CreatedBy: v.UserID,
		Params: map[string]any{
			"action": "detach", "vm_id": vmID, "vm_name": vm.Name,
			"pci_address": address,
		},
	}); err != nil {
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vmID, ResourceName: vm.Name,
		Action:  "vm.passthrough.detach",
		Params:  map[string]any{"pci_address": address},
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// ListVM 返回某台虚拟机挂载的直通设备。
func (s *Service) ListVM(ctx context.Context, vmID int64) ([]model.VMPassthrough, error) {
	var rows []model.VMPassthrough
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).Order("pci_address ASC").Find(&rows).Error; err != nil {
		return nil, api.Internal()
	}
	return rows, nil
}

// --- 内部 ---

// checkGroupConflict 校验同组设备没有挂给别的虚拟机。
func (s *Service) checkGroupConflict(
	ctx context.Context, nodeID, vmID int64, target *DeviceView,
) error {
	if target.IOMUGroup <= 0 {
		return nil
	}
	var rows []model.VMPassthrough
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND iommu_group = ? AND vm_id <> ?",
			nodeID, target.IOMUGroup, vmID).Find(&rows).Error; err != nil {
		return api.Internal()
	}
	if len(rows) == 0 {
		return nil
	}
	names := s.vmNames(ctx, map[string]int64{"addr": rows[0].VMID})
	return api.Conflict("IOMMU 分组 " + itoa(int64(target.IOMUGroup)) +
		" 已经被「" + names[rows[0].VMID] + "」使用了。同一个分组里的设备只能一起直通，" +
		"因此一个分组只能属于一台虚拟机——否则那一台启动时会失败。")
}

func (s *Service) attachedMap(ctx context.Context, nodeID int64) (map[string]int64, error) {
	var rows []model.VMPassthrough
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
		log.Printf("[passthrough] 查询占用失败: %v", err)
		return nil, api.Internal()
	}
	out := make(map[string]int64, len(rows))
	for i := range rows {
		out[rows[i].PCIAddress] = rows[i].VMID
	}
	return out, nil
}

func (s *Service) vmNames(ctx context.Context, attached map[string]int64) map[int64]string {
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(attached))
	for _, id := range attached {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var vms []model.VM
	if err := s.db.WithContext(ctx).Select("id", "name").
		Where("id IN ?", ids).Find(&vms).Error; err != nil {
		log.Printf("[passthrough] 查询虚拟机名失败: %v", err)
		return out
	}
	for _, vm := range vms {
		out[vm.ID] = vm.Name
	}
	return out
}

func (s *Service) loadVM(ctx context.Context, vmID int64) (*model.VM, error) {
	var vm model.VM
	if err := s.db.WithContext(ctx).
		Select("id", "name", "node_id", "status").First(&vm, vmID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("虚拟机不存在")
		}
		return nil, api.Internal()
	}
	return &vm, nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func describeOf(devices []agent.PCIDevice, addr string) string {
	for _, d := range devices {
		if d.Address == addr {
			if d.Description != "" {
				return addr + " " + d.Description
			}
			return addr
		}
	}
	return addr
}

func isDuplicate(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unique") || strings.Contains(s, "duplicate")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
