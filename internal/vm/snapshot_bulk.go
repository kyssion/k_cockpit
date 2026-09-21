package vm

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// firmwareUEFI 与创建/编辑矩阵里 firmware 的取值一致。
const firmwareUEFI = "uefi"

// DeleteAllSnapshotsRequest 是一次「删除全部快照」请求。
type DeleteAllSnapshotsRequest struct {
	// SkipCurrent 为 true 时保留虚拟机当前所处的快照（is_current）。
	//
	// 默认**不**跳过：用户点这个按钮的意图通常是"清空这台机器的还原点"。
	// 留一个开关而不是替他决定，是因为确实存在"只想清掉历史、保留当前"
	// 的场景，而两种意图从界面上看不出区别。
	SkipCurrent bool
}

// DeleteAllSnapshotsView 是受理结果。
type DeleteAllSnapshotsView struct {
	TaskID int64 `json:"task_id"`
	// Total 是本次将删除的快照数；Skipped 是被跳过的（当前快照 / 处理中的）。
	Total   int `json:"total"`
	Skipped int `json:"skipped"`
}

// DeleteAllSnapshots 受理一次「删除全部快照」（F-2-07）。
//
// 它不是一个循环调用单条删除的便捷入口，而是一个**整体操作**：
//
//   - 逐个入队的话，"删到第三个失败"会留下一个既删了一部分、又没有地方
//     可跟踪的中间状态——用户看到三条任务，两条成功一条失败，却不知道
//     剩下那些到底删了没有；
//   - 整体一个任务，进度与失败点都只有一个地方可看，执行器也能按**依赖
//     顺序**删除（先叶后根），而不是按用户碰巧点到的顺序。
//
// 受高风险二次验证保护（见 risk.ActionVMSnapshotDeleteAll）：丢掉一个还原
// 点与丢掉整条时间线不是同一件事。
func (s *Service) DeleteAllSnapshots(
	ctx context.Context, vmID int64, req DeleteAllSnapshotsRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*DeleteAllSnapshotsView, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	var rows []model.VMSnapshot
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).
		Order("created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		log.Printf("[vm] 查询快照失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	skipped := 0
	items := make([]snapshotDeleteItem, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		switch {
		case req.SkipCurrent && r.IsCurrent:
			skipped++
			continue
		case r.Status == model.SnapshotCreating || r.Status == model.SnapshotDeleting:
			// 处理中的快照**跳过而不是拒绝整个操作**：它正在被另一个任务
			// 改动，此时把它列入删除清单会在节点上撞车。
			skipped++
			continue
		}
		items = append(items, snapshotDeleteItem{
			ID: r.ID, Name: r.Name, DomainName: derefStr(r.DomainName),
		})
	}
	if len(items) == 0 {
		return nil, api.Conflict("没有可删除的快照")
	}

	// 顺序：**先删后建的**。快照链上后创建的那些挂在先创建的下游，
	// 反过来删会在第一个上就撞到"存在子快照"而被拒。
	sort.SliceStable(items, func(i, j int) bool { return items[i].ID > items[j].ID })

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMSnapshotDeleteAll,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params: snapshotDeleteAllParams{
			VMID: vm.ID, VMName: vm.Name, Snapshots: items,
		},
	})
	if err != nil {
		return nil, err
	}

	// 标记为删除中：界面立刻能看到"这些正在消失"，而不是等任务跑完才变。
	ids := make([]int64, 0, len(items))
	for i := range items {
		ids = append(ids, items[i].ID)
	}
	if err := s.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id IN ?", ids).
		Updates(map[string]any{"status": model.SnapshotDeleting, "updated_at": time.Now()}).Error; err != nil {
		log.Printf("[vm] 标记快照删除中失败 vm=%d: %v", vmID, err)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:     "vm.snapshot.delete_all.request",
		Params:     map[string]any{"total": len(items), "skipped": skipped, "skip_current": req.SkipCurrent},
		AfterState: map[string]any{"task_id": t.ID},
		Success:    true,
		ClientIP:   clientIP,
	})
	return &DeleteAllSnapshotsView{TaskID: t.ID, Total: len(items), Skipped: skipped}, nil
}

// RepairNVRAM 受理一次 UEFI 启动项修复（F-2-11）。
//
// 触发它的典型场景是**恢复快照之后**：磁盘回到了过去的状态，而 UEFI 固件
// 里记的那条启动项仍然指向当时（或后来）的某个路径，于是机器起不来、直接
// 进 UEFI Shell。磁盘本身没坏——坏的是固件里那一小段记录。
//
// 只对 UEFI 机器有意义，因此 bios 固件时直接拒绝并说明原因：让操作可用但
// 什么也不做，是最容易让人误判"已经修好了"的一类行为。
func (s *Service) RepairNVRAM(
	ctx context.Context, vmID int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	// 固件取值与配置矩阵同源（edit.go 里 firmware 的 Options）。
	if vm.Firmware != firmwareUEFI {
		return nil, api.ValidationFailed(
			"该虚拟机使用 BIOS 启动（当前固件：" + vm.Firmware + "），没有 UEFI 启动项需要修复")
	}
	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMNVRAMRepair,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params: nvramRepairParams{
			VMID: vm.ID, VMName: vm.Name, Firmware: vm.Firmware,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:     "vm.nvram.repair.request",
		Params:     map[string]any{"firmware": vm.Firmware},
		AfterState: map[string]any{"task_id": t.ID},
		Success:    true,
		ClientIP:   clientIP,
	})
	return t, nil
}

// --- 任务参数与执行器 ---

type snapshotDeleteItem struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	DomainName string `json:"domain_name"`
}

type snapshotDeleteAllParams struct {
	VMID      int64                `json:"vm_id"`
	VMName    string               `json:"vm_name"`
	Snapshots []snapshotDeleteItem `json:"snapshots"`
}

type nvramRepairParams struct {
	VMID     int64  `json:"vm_id"`
	VMName   string `json:"vm_name"`
	Firmware string `json:"firmware"`
}

// SnapshotDeleteAllExecutor 逐个删除快照。
type SnapshotDeleteAllExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewSnapshotDeleteAllExecutor 构造执行器。
func NewSnapshotDeleteAllExecutor(db *gorm.DB, client agent.Client) *SnapshotDeleteAllExecutor {
	return &SnapshotDeleteAllExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *SnapshotDeleteAllExecutor) Type() string { return model.TaskVMSnapshotDeleteAll }

// Run 按依赖顺序逐个下发删除。
//
// 中途失败**立即返回错误**，而不是继续删剩下的：继续的话会留下一个"删了
// 一半、且不知道哪半"的状态；停在那里，任务详情里能明确看到卡在第几个。
func (e *SnapshotDeleteAllExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p snapshotDeleteAllParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析批量删除快照参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	for i := range p.Snapshots {
		s := &p.Snapshots[i]
		result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
			Kind:   agent.OpVMSnapshotDelete,
			NodeID: *t.NodeID,
			Target: p.VMName,
			Params: map[string]any{"snapshot_name": s.Name, "domain_name": s.DomainName},
		})
		if err != nil {
			return api.Unavailable("节点不可达，快照未删除：" + s.Name)
		}
		if !result.Success {
			return api.ValidationFailed("删除快照「" + s.Name + "」失败：" + result.Message)
		}
		if err := e.db.WithContext(ctx).
			Where("id = ?", s.ID).Delete(&model.VMSnapshot{}).Error; err != nil {
			// 节点上已经删掉了，控制面这条记录留着会表现为"删了还在"。
			// 记日志并继续：为它把整个任务判失败，会让用户以为删除没生效。
			log.Printf("[vm] 删除快照记录失败 id=%d: %v", s.ID, err)
		}
	}
	return nil
}

// NVRAMRepairExecutor 下发 UEFI 启动项修复。
type NVRAMRepairExecutor struct {
	agent agent.Client
}

// NewNVRAMRepairExecutor 构造执行器。
func NewNVRAMRepairExecutor(client agent.Client) *NVRAMRepairExecutor {
	return &NVRAMRepairExecutor{agent: client}
}

// Type 返回处理的任务类型。
func (e *NVRAMRepairExecutor) Type() string { return model.TaskVMNVRAMRepair }

// Run 下发修复指令。
func (e *NVRAMRepairExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p nvramRepairParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析 NVRAM 修复参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMNVRAMRepair,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{"firmware": p.Firmware},
	})
	if err != nil {
		return api.Unavailable("节点不可达，启动项未修复")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	return nil
}
