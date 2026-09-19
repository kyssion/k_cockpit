package vm

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// DiskResizeResult 是扩容结果。
type DiskResizeResult struct {
	Task *model.Task `json:"task"`
	// OldGB / NewGB 是扩容前后的容量。
	OldGB int `json:"old_gb"`
	NewGB int `json:"new_gb"`
	// GuestGrowNeeded 为 true 表示**来宾里还需要扩分区与文件系统**。
	//
	// 必须显式告诉用户：宿主机侧扩完只是"盘子变大了"，而操作系统看到的
	// 仍然是原来的分区。不提醒的话，用户会以为扩容失败。
	GuestGrowNeeded bool `json:"guest_grow_needed"`
}

// ResizeDisk 把虚拟机的系统盘扩容到指定容量。
//
// 三条约束，每一条都对应一类真实的事故：
//
//  1. **只能扩，不能缩。** 缩容会**丢数据**：镜像文件变小之后，文件系统里
//     超出新边界的那些块还在原地，但已经不属于这个设备了——而文件系统
//     自己不知道。它的下一次写入就可能覆盖掉别的东西。这不是"有风险"，
//     是"一定会坏"。
//
//  2. **要求关机。** 运行中的磁盘扩容在宿主机侧是可以做的（热插拔 + 内核
//     重新读取容量），但**来宾里仍然要扩分区**——而那是 guest agent 那条
//     路径会顺带做的事。两条路径各做一半的话，用户得到的是"盘大了但用不了"
//     ——正是这个功能要消灭的那种机器。因此这里直接要求关机，并在报错里
//     **指向另一个入口**。
//
//  3. **宿主机侧扩完不等于可用。** 来宾里的分区与文件系统还要扩，而做法
//     依系统不同。界面上必须把这一步说出来（见 GuestGrowNeeded）。
func (s *Service) ResizeDisk(
	ctx context.Context, id int64, newGB int, v authz.Viewer, operatorName, clientIP string,
) (*DiskResizeResult, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	if newGB <= 0 {
		return nil, api.InvalidParameter("容量必须大于 0")
	}
	// 上限与创建时一致：一个填错的数字（比如把 GB 当成 MB）在这里的后果
	// 是创建了一个几千 TB 的稀疏文件——它看起来只占一点空间，直到写满。
	if newGB > 65536 {
		return nil, api.InvalidParameter("容量不能超过 65536 GB（64 TB）——请确认单位是 GB")
	}

	if newGB == vm.DiskGB {
		// 相同的容量**拒绝**而不是静默成功：一次没有任何变化的扩容会在
		// 节点上跑一遍完整流程，而用户以为自己做了什么。
		return nil, api.ValidationFailed("新容量与当前容量相同")
	}
	if newGB < vm.DiskGB {
		// 见约束一。文案要说清"为什么"，而不只是"不支持"——用户很可能
		// 本来就以为缩容是可以的（在别的地方见过）。
		return nil, api.ValidationFailed(
			"磁盘只能扩大，不能缩小。缩容会让镜像里超出新边界的那些数据" +
				"仍留在原地而文件系统不再知道自己拥有它们——下一次写入就可能" +
				"覆盖别的内容。需要更小的盘请新建一台并用迁移的方式搬过去。")
	}
	if vm.Status == model.VMStatusRunning {
		// 见约束二。**指向另一个入口**：用户想做的事（扩盘）在那边能做，
		// 而只告诉他"要关机"会让他去关机再回来——那也能成，但多了一步，
		// 而且他在这一步里会怀疑自己是不是搞错了。
		return nil, api.Conflict(
			"虚拟机正在运行。运行中的扩容请在「来宾自动化」里用「扩容磁盘」" +
				"——它会在扩完之后**顺带在来宾里扩好文件系统**。这里的入口用于" +
				"关机状态的机器，扩完之后还需要你进来宾自己扩分区。")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDiskResize,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      derefOwner(vm.OwnerID),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id": vm.ID, "vm_name": vm.Name,
			"old_gb": vm.DiskGB, "new_gb": newGB,
			"disk_format": vm.DiskFormat,
		},
	})
	if err != nil {
		// **不要把入队错误吞成 Internal。**
		//
		// 入队失败有一类明确的原因（任务类型没注册、参数不合法），而它们
		// 的原始文案是能直接指出问题的。换成一句「服务内部错误」之后，
		// 用户与排查的人都只能看到"失败了"，而真正的原因被丢掉了——
		// 这个错误就是在写这段代码时踩到的：任务类型没注册，本该得到
		// 「不支持的任务类型」，实际得到的是「服务内部错误」。
		return nil, err
	}

	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vm.ID).Update("disk_gb", newGB).Error; err != nil {
		log.Printf("[vm] 更新磁盘容量失败 id=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.disk.resize",
		Params: map[string]any{
			"old_gb": vm.DiskGB, "new_gb": newGB,
			"note": "仅扩宿主机侧；来宾内分区与文件系统需自行扩展",
		},
		Success: true, ClientIP: clientIP,
	})

	return &DiskResizeResult{
		Task: t, OldGB: vm.DiskGB, NewGB: newGB,
		// **由服务端给出而不是让界面自己填一句固定文案**：这一步是否必要
		// 取决于扩容路径，而那是服务端才知道的事。
		GuestGrowNeeded: true,
	}, nil
}

// DiskResizeExecutor 执行关机状态下的磁盘扩容。
type DiskResizeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDiskResizeExecutor 构造执行器。
func NewDiskResizeExecutor(db *gorm.DB, client agent.Client) *DiskResizeExecutor {
	return &DiskResizeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DiskResizeExecutor) Type() string { return model.TaskVMDiskResize }

// Run 下发扩容。
func (e *DiskResizeExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析扩容参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMDiskResize,
		NodeID: nodeID,
		Target: strOf(p["vm_name"]),
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，磁盘未扩容")
	}
	if !result.Success {
		// **失败时把记录改回去。**
		//
		// 与别处"失败保留状态"不同：磁盘容量是我们自己的记录，而节点上
		// 那张盘并没有变大。留着新值会让界面显示一个不存在的容量——用户
		// 看到"500 GB"而去来宾里找那 300 GB，找不到。
		if newGB, ok := numberAsInt(p["new_gb"]); ok {
			if oldGB, ok2 := numberAsInt(p["old_gb"]); ok2 {
				if err := e.db.WithContext(ctx).Model(&model.VM{}).
					Where("id = ? AND disk_gb = ?", t.ResourceID, newGB).
					Update("disk_gb", oldGB).Error; err != nil {
					log.Printf("[vm] 回滚磁盘容量失败 vm=%d: %v", t.ResourceID, err)
				}
			}
		}
		return api.ValidationFailed(result.Message)
	}
	return nil
}

func numberAsInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	}
	return 0, false
}

// derefOwner 把可空的归属转成任务入队需要的值（0 表示无归属）。
func derefOwner(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}
