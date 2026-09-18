package platformcheck

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// Executor 执行按期望状态的重新下发。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskPlatformRepair }

// Run 下发。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[platformcheck] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPlatformRepair,
		NodeID: nodeID,
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，未执行修复")
	}
	if !result.Success {
		// **部分修复也要如实报出来。**
		//
		// 修复的能力受限于节点（缺 OVS 装不上），因此"部分成功"是常态。
		// 笼统地回一个"已修复"会让用户以为自检里的偏差都清了，而下一轮
		// 自检还会看到它们——那时他会开始怀疑自检本身不准。
		return api.ValidationFailed(result.Message)
	}
	return nil
}
