package hosttuning

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// Executor 执行调优项的下发。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskHostTuning }

// Run 下发。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[hosttuning] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpHostTuningApply,
		NodeID: nodeID,
		Target: strOf(p["item"]),
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，调优未生效")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	return nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}
