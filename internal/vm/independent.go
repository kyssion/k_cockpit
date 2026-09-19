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

// MakeDisksIndependent 把链接克隆的磁盘变成独立盘。
//
// **它存在的理由是一件事：链接克隆的父盘删不掉。**
//
// 链接克隆的磁盘只是模板之上的一层覆盖——它记录的是"相对模板改了什么"。
// 这让克隆很快、很省空间，代价是那台机器**永远依赖着父盘**：父盘一没，
// 它就废了。而模板的管理（更新、清理、下线）需要能删掉旧的父盘。
//
// 三条约束：
//
//  1. **只对链接克隆有效。** 一个已经是独立盘的机器再"解除依赖"是无意义
//     操作，而静默成功会让用户以为"刚才那步做了什么"。
//
//  2. **必须停机。** 它要复制整个镜像（可能几十 GB），而运行中的机器还在
//     往那层覆盖里写——复制出来的东西与"某一瞬间"对不上。
//
//  3. **失败的时机决定标记怎么改。** 标记**只能在节点确认成功之后**才改：
//     先改成 full 的话，控制面会说"这台机器已经独立"而实际它还依赖着父盘。
//     那时用户去删模板——删掉了——然后一堆机器坏掉，而没有任何地方提示过
//     它们还在依赖。这是这个操作里唯一一处会**静默造成大面积损坏**的路径。
func (s *Service) MakeDisksIndependent(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 见约束一。文案要说清"当前是什么状态"，而不只是一句"不支持"。
	if vm.CloneMode != model.CloneLinked {
		return nil, api.ValidationFailed(
			"这台虚拟机不是链接克隆，它的磁盘已经是独立的——没有需要解除的依赖")
	}
	if vm.Status == model.VMStatusRunning {
		return nil, api.Conflict(
			"需要先关机。合并要复制整个镜像，而运行中的机器还在往那层覆盖里写——" +
				"复制出来的内容会与任何一个时刻都对不上")
	}

	freedFrom := ""
	if vm.TemplateID != nil {
		var tpl model.Template
		if err := s.db.WithContext(ctx).Select("name").
			First(&tpl, *vm.TemplateID).Error; err == nil {
			freedFrom = tpl.Name
		}
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDisksIndependent,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      derefOwner(vm.OwnerID),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id": vm.ID, "vm_name": vm.Name,
			"template_id": vm.TemplateID, "freed_from": freedFrom,
		},
	})
	if err != nil {
		// 原样返回入队错误——见 ResizeDisk 里的同一条说明。
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.disks.independent",
		Params: map[string]any{
			"freed_from": freedFrom,
			"note":       "磁盘已从链接克隆合并为独立镜像；父盘随后可被删除",
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// IndependentExecutor 执行磁盘合并。
type IndependentExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewIndependentExecutor 构造执行器。
func NewIndependentExecutor(db *gorm.DB, client agent.Client) *IndependentExecutor {
	return &IndependentExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *IndependentExecutor) Type() string { return model.TaskVMDisksIndependent }

// Run 下发并**在成功之后**才改标记。
func (e *IndependentExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析解除依赖参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMDisksIndependent,
		NodeID: nodeID,
		Target: strOf(p["vm_name"]),
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，磁盘未合并")
	}
	if !result.Success {
		// **失败时保持 linked**。见方法注释里的约束三：标记先改的话，控制面
		// 会说"已经独立"而实际还依赖着父盘——用户去删模板，然后一堆机器坏掉。
		return api.ValidationFailed(result.Message)
	}

	if err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", t.ResourceID).
		Updates(map[string]any{
			"clone_mode": model.CloneFull,
			// **保留 TemplateID**：它记录的是"这台机器从哪来"，而那是历史。
			// 清掉的话，界面上再也说不清这台机器的来源，而排查时
			// "它是从哪个模板做的"往往正是要找的东西。
		}).Error; err != nil {
		log.Printf("[vm] 更新克隆模式失败 vm=%d: %v", t.ResourceID, err)
	}
	return nil
}
