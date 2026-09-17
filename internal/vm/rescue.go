package vm

import (
	"context"
	"encoding/json"
	"log"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// rescueSnapshot 是进入救援前保存的配置。
//
// 只保存**救援过程会改动的字段**（f-2-12：改盘型/网卡/引导顺序）。保存整份
// 配置会把「用户在这期间通过编辑页改的其它字段」也一起回滚掉——那不是
// 「还原救援」，而是「还原到进入救援的那一刻」，两回事。
type rescueSnapshot struct {
	BootOrder     string `json:"boot_order"`
	MachineType   string `json:"machine_type"`
	DisplayDevice string `json:"display_device"`
	Firmware      string `json:"firmware"`
	SecureBoot    bool   `json:"secure_boot"`
}

func snapshotOf(vm *model.VM) rescueSnapshot {
	return rescueSnapshot{
		BootOrder:     vm.BootOrder,
		MachineType:   vm.MachineType,
		DisplayDevice: vm.DisplayDevice,
		Firmware:      vm.Firmware,
		SecureBoot:    vm.SecureBoot,
	}
}

// EnterRescue 让虚拟机从救援镜像启动（F-2-12）。
//
// **必须关机**：救援是对硬件配置的改动（引导顺序、盘型），热改会让控制面
// 记录的配置与虚拟化层实际的配置分叉——而这份分叉在退出还原时会变成一个
// 「还原成什么样」说不清的状态。
//
// 进入前保存配置快照：退出时按它还原。没有快照就只能猜一个默认值填回去，
// 那是悄悄改掉用户的配置——他进入救援是为了修系统，退出后发现引导顺序被
// 重置了，机器起不来。
func (s *Service) EnterRescue(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	if vm.RescueActive {
		return nil, api.ValidationFailed("该虚拟机已处于救援模式")
	}

	// 与删除、电源操作一致：以**实时探测**为准，不用投影（f-2-01 R-002）。
	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	if current != model.VMStatusStopped {
		return nil, api.ValidationFailed(
			"进入救援需要先关机（当前：" + DescribeStatus(current) + "）")
	}

	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	snap, err := json.Marshal(snapshotOf(vm))
	if err != nil {
		log.Printf("[vm] 序列化救援快照失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}
	snapshot := string(snap)

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMRescueEnter,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id":   vm.ID,
			"vm_name": vm.Name,
			// 快照随任务一起持久化，而不是在受理时先写库：写入库意味着
			// 任务失败时库里存着一份没有对应救援状态的快照，而退出时
			// 会拿它去还原一台从未进入过救援的机器。
			"snapshot":        snapshot,
			"observed_status": current,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "vm.rescue.enter",
		Params: map[string]any{"task_id": t.ID}, Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// ExitRescue 退出救援并按快照还原配置（F-2-12）。
func (s *Service) ExitRescue(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	if !vm.RescueActive {
		return nil, api.ValidationFailed("该虚拟机未处于救援模式")
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	if current != model.VMStatusStopped {
		return nil, api.ValidationFailed(
			"退出救援需要先关机（当前：" + DescribeStatus(current) + "）")
	}

	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	snapshot := ""
	if vm.RescueConfig != nil {
		snapshot = *vm.RescueConfig
	}
	if snapshot == "" {
		// 快照缺失（历史数据或写入中断）时**拒绝退出**而不是用默认值还原。
		//
		// 用默认值「还原」会静默改掉用户的引导顺序与固件设置，而他无从
		// 判断这是不是自己原来的配置——相比起来，「需要人工确认」是一个
		// 明确得多、也安全得多的结果。
		return nil, api.ValidationFailed(
			"缺少进入救援时的配置快照，无法自动还原；请通过编辑页手动确认引导顺序等配置")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMRescueExit,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id":           vm.ID,
			"vm_name":         vm.Name,
			"snapshot":        snapshot,
			"observed_status": current,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "vm.rescue.exit",
		Params: map[string]any{"task_id": t.ID}, Success: true, ClientIP: clientIP,
	})
	return t, nil
}
