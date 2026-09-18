package quotaenforce

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// enforceParams 是 quota.enforce 任务的参数。
type enforceParams struct {
	QuotaID   int64  `json:"quota_id"`
	UserID    int64  `json:"user_id"`
	Dimension string `json:"dimension"`
	// Enforce 为 true 表示施加处置，false 表示撤销。
	Enforce bool   `json:"enforce"`
	Action  string `json:"action"`
}

// Executor 执行配额处置的下发。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskQuotaEnforce }

// Run 下发期望状态。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p enforceParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[quotaenforce] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpQuotaEnforce,
		NodeID: nodeID,
		Target: p.Dimension,
		Params: map[string]any{
			"user_id":   p.UserID,
			"dimension": p.Dimension,
			"enforce":   p.Enforce,
			"action":    p.Action,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，配额处置未生效")
	}
	if !result.Success {
		// **失败保留状态**：控制面记的是"应该限速"，而节点上没生效。
		// 把状态改回去会掩盖这个不一致；下一次判定会重试——它天然幂等。
		return api.ValidationFailed(result.Message)
	}
	return nil
}
