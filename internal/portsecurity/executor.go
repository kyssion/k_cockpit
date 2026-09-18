package portsecurity

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

// policyParams 是 port_security.apply 任务的参数。
type policyParams struct {
	PolicyID int64  `json:"policy_id"`
	PortRef  string `json:"port_ref"`
	VMName   string `json:"vm_name"`

	// 期望状态。
	SpoofingGuard bool `json:"spoofing_guard"`
	Isolation     bool `json:"isolation"`
	PPSLimit      int  `json:"pps_limit"`
}

// Executor 执行端口安全策略的下发。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskPortSecurityApply }

// Run 下发期望状态并同步记录结果。
//
// 顺序是**先让节点成功、再改记录**：反过来的话，节点失败时控制面会认为
// 策略已经生效——而用户会因此以为攻击面已经收住了。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p policyParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[portsecurity] 解析任务参数失败: %v", err)
		return api.Internal()
	}

	// 节点侧自己也不需要参数里的 node_id：它就是从那条通道来的。
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}
	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPortSecurityApply,
		NodeID: nodeID,
		Target: p.PortRef,
		Params: map[string]any{
			"port_ref":       p.PortRef,
			"vm_name":        p.VMName,
			"spoofing_guard": p.SpoofingGuard,
			"isolation":      p.Isolation,
			"pps_limit":      p.PPSLimit,
		},
	})
	if err != nil {
		e.mark(ctx, p.PolicyID, model.PortSecurityFailed, "节点不可达")
		return api.Unavailable("节点不可达，端口安全未生效")
	}
	if !result.Success {
		e.mark(ctx, p.PolicyID, model.PortSecurityFailed, result.Message)
		return api.ValidationFailed(result.Message)
	}

	// 关掉全部保护时，状态回到 pending 而不是 active。
	//
	// 「已生效」对一份什么都不做的策略是没有意义的——它描述的是一组已经
	// 写下去的规则，而这里恰恰一条都没有。
	if !p.SpoofingGuard && !p.Isolation && p.PPSLimit == 0 {
		e.mark(ctx, p.PolicyID, model.PortSecurityPending, "已停用全部保护")
		return nil
	}

	now := time.Now()
	if err := e.db.WithContext(ctx).Model(&model.PortSecurityPolicy{}).
		Where("id = ?", p.PolicyID).
		Updates(map[string]any{
			"status":     model.PortSecurityActive,
			"applied_at": now,
			"detail":     nil,
		}).Error; err != nil {
		log.Printf("[portsecurity] 更新策略状态失败 id=%d: %v", p.PolicyID, err)
	}
	return nil
}

// mark 记录失败原因。
//
// 失败**保留记录**（不删）：与目录共享不同，策略是用户配置的意图，
// 而且失败之后他还需要看到它、改它、重试——删掉记录等于让他重新配一遍，
// 而他并不知道自己刚配的那份去哪了。
func (e *Executor) mark(ctx context.Context, id int64, status, detail string) {
	if id == 0 {
		return
	}
	if err := e.db.WithContext(ctx).Model(&model.PortSecurityPolicy{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": status, "detail": detail}).Error; err != nil {
		log.Printf("[portsecurity] 记录策略状态失败 id=%d: %v", id, err)
	}
}
