package vm

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
)

// CDROMExecutor 执行光驱的五种动作。
type CDROMExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewCDROMExecutor 构造执行器。
func NewCDROMExecutor(db *gorm.DB, client agent.Client) *CDROMExecutor {
	return &CDROMExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *CDROMExecutor) Type() string { return model.TaskVMCDROMApply }

// Run 下发。
func (e *CDROMExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析光驱参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMCDROMApply,
		NodeID: nodeID,
		Target: strOf(p["vm_name"]),
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，光驱配置未变更")
	}
	if !result.Success {
		// 失败时**记录保持不变**：控制面记的是"应该是什么样"，而节点上没生效。
		// 把记录改回去会掩盖这个不一致，而用户下一次看到的是"我明明改了"。
		// 下一次操作会重发完整状态——这个操作是幂等的。
		return api.ValidationFailed(result.Message)
	}

	// 只有换总线需要在成功后回写记录：它是**唯一**一个受理时不能先改库的
	// 动作（节点上是 detach + attach 两步，失败则域里仍是旧设备）。其余动作
	// 的记录在受理时就已写好，语义是"控制面希望的状态"。
	if strOf(p["action"]) == "bus" {
		e.markBusApplied(ctx, p)
	}
	return nil
}

// markBusApplied 把新总线写回光驱记录；记录已不存在时只记日志。
func (e *CDROMExecutor) markBusApplied(ctx context.Context, p map[string]any) {
	id, ok := specInt64(p, "cdrom_id")
	bus := strOf(p["bus"])
	if !ok || id <= 0 || bus == "" {
		log.Printf("[vm] 换总线任务缺少 cdrom_id/bus，跳过回写: %#v", p)
		return
	}
	res := e.db.WithContext(ctx).Model(&model.VMCDROM{}).
		Where("id = ?", id).Update("bus", bus)
	if res.Error != nil {
		// 域里已经换好了：回写失败不影响结果，只记日志。下一次换总线或列表
		// 刷新会暴露这条记录，而让任务失败会让用户以为节点上没生效。
		log.Printf("[vm] 回写光驱总线失败 id=%d bus=%s: %v", id, bus, res.Error)
	}
}
