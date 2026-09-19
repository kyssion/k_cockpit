package vm

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
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
	return nil
}
