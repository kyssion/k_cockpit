package vm

import (
	"context"
	"encoding/json"
	"log"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// --- PCIe 根端口 ---

// PCIeInfoView 是详情页展示的 PCIe 根端口情况。
type PCIeInfoView struct {
	VMID        int64  `json:"vm_id"`
	Total       int    `json:"total"`
	Free        int    `json:"free"`
	Used        int    `json:"used"`
	MachineType string `json:"machine_type"`
	// HotplugSupported 为 false 时 Reason 会说明原因（通常是 i440FX）。
	HotplugSupported bool   `json:"hotplug_supported"`
	Reason           string `json:"reason,omitempty"`
	// Unavailable 非空表示节点没实现这个操作。
	Unavailable string `json:"unavailable,omitempty"`
}

// PCIeInfo 读取一台虚拟机的 PCIe 根端口余量。
//
// 它在**详情页**按需取而不是随详情一起返回：这是一次向节点的请求，塞进
// 详情会让每次打开详情页都多一次往返，而多数时候用户并不关心槽位。
func (s *Service) PCIeInfo(ctx context.Context, id int64, v authz.Viewer) (*PCIeInfoView, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	out := &PCIeInfoView{VMID: vm.ID, MachineType: vm.MachineType}
	if s.agent == nil {
		out.Unavailable = "未连接节点"
		return out, nil
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMPcieInfo,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil || result == nil || !result.Success || result.Data == nil {
		out.Unavailable = "节点未提供 PCIe 信息"
		return out, nil
	}
	info := decodePCIeInfo(result.Data)
	if info.Total == 0 && info.Reason == "" {
		out.Unavailable = "节点未提供 PCIe 信息"
		return out, nil
	}
	out.Total = info.Total
	out.Free = info.Free
	out.Used = info.Total - info.Free
	if info.MachineType != "" {
		out.MachineType = info.MachineType
	}
	out.HotplugSupported = info.HotplugSupported
	out.Reason = info.Reason
	return out, nil
}

func decodePCIeInfo(data map[string]any) agent.PCIeInfo {
	raw, ok := data[agent.PCIeDataKey]
	if !ok {
		return agent.PCIeInfo{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.PCIeInfo{}
	}
	var info agent.PCIeInfo
	_ = json.Unmarshal(blob, &info)
	return info
}

// --- 邻居表 ---

// NeighborView 是一条邻居记录。
type NeighborView struct {
	IP        string `json:"ip"`
	MAC       string `json:"mac,omitempty"`
	Interface string `json:"interface,omitempty"`
	State     string `json:"state,omitempty"`
	IsSelf    bool   `json:"is_self"`
	VMID      *int64 `json:"vm_id,omitempty"`
	VMName    string `json:"vm_name,omitempty"`
	Bridge    string `json:"bridge,omitempty"`
}

// NeighborsView 是邻居表查询的结果。
type NeighborsView struct {
	VMID  int64          `json:"vm_id"`
	Items []NeighborView `json:"items"`
	// Unavailable 非空表示节点没实现；此时界面给的是"读不到"而不是"没有邻居"。
	Unavailable string `json:"unavailable,omitempty"`
}

// Neighbors 读取一台虚拟机所在二层网络的邻居表。
func (s *Service) Neighbors(ctx context.Context, id int64, v authz.Viewer) (*NeighborsView, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	out := &NeighborsView{VMID: vm.ID, Items: []NeighborView{}}
	if s.agent == nil {
		out.Unavailable = "未连接节点"
		return out, nil
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMNeighbors,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil || result == nil || !result.Success || result.Data == nil {
		out.Unavailable = "节点未提供邻居表"
		return out, nil
	}
	rows := decodeNeighbors(result.Data)
	if len(rows) == 0 {
		out.Unavailable = "节点未提供邻居表"
		return out, nil
	}

	// 把 MAC 对应到面板里的虚拟机：用户拿 MAC 去比对没人名有用。
	macs := make([]string, 0, len(rows))
	for i := range rows {
		if rows[i].MAC != "" {
			macs = append(macs, rows[i].MAC)
		}
	}
	names := s.interfacesByMAC(ctx, vm.NodeID, macs)

	for i := range rows {
		r := &rows[i]
		view := NeighborView{
			IP: r.IP, MAC: r.MAC, Interface: r.Interface, State: r.State, IsSelf: r.IsSelf, Bridge: r.Bridge,
		}
		if n, ok := names[r.MAC]; ok {
			view.VMID = &n.ID
			view.VMName = n.Name
		}
		out.Items = append(out.Items, view)
	}
	return out, nil
}

// macOwner 是 MAC → 虚拟机 的映射结果。
type macOwner struct {
	ID   int64
	Name string
}

// interfacesByMAC 按 MAC 找到对应的虚拟机（只在同一节点内找）。
func (s *Service) interfacesByMAC(ctx context.Context, nodeID int64, macs []string) map[string]macOwner {
	out := map[string]macOwner{}
	if len(macs) == 0 {
		return out
	}
	var rows []model.VMInterface
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND mac IN ?", nodeID, macs).
		Select("id", "vm_id", "mac").Find(&rows).Error; err != nil {
		log.Printf("[vm] 按 MAC 查询网口失败: %v", err)
		return out
	}
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].VMID)
	}
	if len(ids) == 0 {
		return out
	}
	var vms []model.VM
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).
		Select("id", "name").Find(&vms).Error; err != nil {
		log.Printf("[vm] 查询虚拟机名失败: %v", err)
		return out
	}
	names := map[int64]string{}
	for i := range vms {
		names[vms[i].ID] = vms[i].Name
	}
	for _, r := range rows {
		if r.MAC == nil {
			continue
		}
		if n, ok := names[r.VMID]; ok {
			out[*r.MAC] = macOwner{ID: r.VMID, Name: n}
		}
	}
	return out
}

func decodeNeighbors(data map[string]any) []agent.NeighborEntry {
	raw, ok := data[agent.NeighborDataKey]
	if !ok {
		return nil
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var rows []agent.NeighborEntry
	if err := json.Unmarshal(blob, &rows); err != nil {
		return nil
	}
	return rows
}

// --- 事件时间线 ---

// TimelineItem 是详情页时间线上的一条。
//
// 它把**审计**与**任务**两条流水合成一条：用户问的是"这台机器发生过什么"，
// 而不是"审计里有什么"。分开两个列表会让他自己按时间去对。
type TimelineItem struct {
	At     string `json:"at"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Success 仅在任务类条目有意义；审计类为 nil。
	Success *bool  `json:"success,omitempty"`
	TaskID  *int64 `json:"task_id,omitempty"`
}

// Timeline 返回一台虚拟机的事件时间线（合并审计与任务）。
func (s *Service) Timeline(ctx context.Context, id int64, v authz.Viewer, limit int) ([]TimelineItem, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	out := make([]TimelineItem, 0, limit)

	var tasks []model.Task
	if err := s.db.WithContext(ctx).
		Where("resource_type = ? AND resource_id = ?", "vm", vm.ID).
		Order("created_at DESC").Limit(limit).Find(&tasks).Error; err != nil {
		log.Printf("[vm] 查询任务流水失败 vm=%d: %v", vm.ID, err)
		// 不返回错误：任务流水只是时间线的一半，审计那一半仍要给出来。
	}
	for i := range tasks {
		t := &tasks[i]
		ok := t.Status == model.TaskSuccess
		item := TimelineItem{
			At:      t.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			Kind:    "task",
			Title:   taskTitle(t.Type, t.Status),
			Success: &ok,
		}
		id := t.ID
		item.TaskID = &id
		out = append(out, item)
	}

	var logs []model.AuditLog
	if err := s.db.WithContext(ctx).
		Where("resource_type = ? AND resource_id = ?", "vm", vm.ID).
		Order("at DESC").Limit(limit).Find(&logs).Error; err != nil {
		log.Printf("[vm] 查询审计流水失败 vm=%d: %v", vm.ID, err)
	}
	for i := range logs {
		l := &logs[i]
		out = append(out, TimelineItem{
			At:     l.At.Format("2006-01-02T15:04:05Z07:00"),
			Kind:   "audit",
			Title:  auditTitle(l.Action),
			Detail: auditDetail(l),
		})
	}

	// 时间倒序：最近的在最上面。
	sortByTimeDesc(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func taskTitle(taskType, status string) string {
	switch taskType {
	case model.TaskVMCreate:
		return "创建虚拟机"
	case model.TaskVMPower:
		return "电源操作"
	case model.TaskVMDelete:
		return "删除虚拟机"
	case model.TaskVMSnapshotCreate:
		return "创建快照"
	case model.TaskVMSnapshotRestore:
		return "恢复快照"
	case model.TaskVMSnapshotDelete:
		return "删除快照"
	case model.TaskVMConfigUpdate:
		return "修改配置"
	case model.TaskVMDiskResize:
		return "扩容磁盘"
	case model.TaskVMMigrate:
		return "迁移"
	case model.TaskVMSnapshotDeleteAll:
		return "删除全部快照"
	default:
		return taskType + "（" + status + "）"
	}
}

func auditTitle(action string) string {
	switch action {
	case "vm.snapshot.create.request":
		return "创建快照"
	case "vm.snapshot.restore.request":
		return "恢复快照"
	case "vm.tags.set":
		return "更新标签"
	case "vm.interface.add.request":
		return "新增网口"
	case "vm.disk.change":
		return "磁盘变更"
	case "vm.nvram.repair.request":
		return "修复 UEFI 启动项"
	default:
		return action
	}
}

func auditDetail(l *model.AuditLog) string {
	if l.Error != nil && *l.Error != "" {
		return *l.Error
	}
	if l.OperatorName != nil && *l.OperatorName != "" {
		return "操作人 " + *l.OperatorName
	}
	return ""
}

func sortByTimeDesc(items []TimelineItem) {
	// 字符串形式的 RFC3339 可直接字典序比较（同格式同时区）。
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].At > items[j-1].At; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
