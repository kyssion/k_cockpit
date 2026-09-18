package hostfirewall

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// Executor 执行宿主机防火墙的下发与回滚。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskHostFirewallApply }

// Run 下发。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[hostfirewall] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpHostFirewallApply,
		NodeID: nodeID,
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，宿主机防火墙未生效")
	}
	if !result.Success {
		// **失败不谎报成功**：控制面上显示"已应用"而宿主机上没生效，会让
		// 管理员以为自己受保护着，而实际没有——这比明说失败危险得多。
		return api.ValidationFailed(result.Message)
	}

	// 成功之后才标记已应用，理由同上。
	if action, _ := p["action"].(string); action == "apply" {
		if err := e.db.WithContext(ctx).Model(&model.HostFirewallRule{}).
			Where("node_id = ?", nodeID).Update("applied", true).Error; err != nil {
			log.Printf("[hostfirewall] 标记规则已应用失败: %v", err)
		}
		if err := e.db.WithContext(ctx).Model(&model.HostFirewallPolicy{}).
			Where("node_id = ?", nodeID).Update("applied_at", time.Now()).Error; err != nil {
			log.Printf("[hostfirewall] 标记策略已应用失败: %v", err)
		}
	}
	return nil
}
