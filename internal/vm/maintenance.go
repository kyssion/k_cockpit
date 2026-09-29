package vm

import (
	"context"
	"log"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/task"
)

// ShutdownAllOnNode 为节点上投影状态为「运行中」的虚拟机逐台入队优雅关机
// 任务，返回入队数。供站点维护模式使用（G-46）。
//
// 逐台入队而不是一个批量任务：每台机器的关机进度、失败与重试都要在任务
// 中心独立可见——站点维护是「关一批机器」的动作，而排障时要回答的是
// 「为什么偏偏这台没关掉」。
//
// 用投影状态而不是逐台探测：这里是**批量受理**，逐台探测会把一次维护
// 变成 N 次跨节点往返；探测的意义在单台操作时（判定该不该执行），批量
// 场景下任务执行器自会与节点上的真实状态对账，个别机器在受理后状态变化
// 的，任务会如实失败而不是被静默跳过。
//
// 只用优雅关机（shutdown），不用强制断电：强杀可能造成来宾文件系统损坏，
// 而「维护之前要不要强关」是用户的决定，系统不代做（f-2-01 R-006）。
func (s *Service) ShutdownAllOnNode(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (int, error) {
	var vms []model.VM
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND status = ? AND present = ?", nodeID, model.VMStatusRunning, true).
		Find(&vms).Error
	if err != nil {
		log.Printf("[vm] 查询节点上的运行中虚拟机失败 node_id=%d: %v", nodeID, err)
		return 0, api.Internal()
	}

	enqueued := 0
	for i := range vms {
		m := &vms[i]
		params := powerParams{
			VMID: m.ID, VMName: m.Name,
			Action:         string(PowerShutdown),
			ObservedStatus: m.Status,
		}
		if _, err := s.queue.Enqueue(ctx, task.Spec{
			Type:         model.TaskVMPower,
			NodeID:       nodeID,
			ResourceType: "vm",
			ResourceID:   m.ID,
			ResourceName: m.Name,
			OwnerID:      ownerOf(m, v),
			CreatedBy:    v.UserID,
			Params:       params,
		}); err != nil {
			// 单台失败不中断整批：维护的目标是「尽快让节点安静下来」，
			// 一台入队失败就把其余机器全部留在运行中没有道理。
			log.Printf("[vm] 站点维护关机入队失败 vm=%s: %v", m.Name, err)
			continue
		}
		enqueued++

		s.record(ctx, audit.Entry{
			OperatorID:   v.UserID,
			OperatorName: operatorName,
			NodeID:       nodeID,
			ResourceType: "vm",
			ResourceID:   m.ID,
			ResourceName: m.Name,
			Action:       "vm.power.request",
			Params:       map[string]any{"action": string(PowerShutdown), "observed_status": m.Status, "bulk": "site_maintenance"},
			BeforeState:  map[string]any{"status": m.Status},
			Success:      true,
			ClientIP:     clientIP,
		})
	}
	return enqueued, nil
}
