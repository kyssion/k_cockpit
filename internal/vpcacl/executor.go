package vpcacl

import (
	"context"
	"encoding/json"
	"log"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

type aclParams struct {
	NodeID   int64                  `json:"node_id"`
	SwitchID *int64                 `json:"switch_id"`
	Version  string                 `json:"version"`
	Spec     []agent.VpcACLRuleSpec `json:"spec"`
}

// Executor 应用 ACL 规则集。
type Executor struct {
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(client agent.Client) *Executor { return &Executor{agent: client} }

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskVpcACLApply }

// Run 下发规则集。
//
// 传的是**整个规则集**而不是增量：ACL 的语义是"从上往下匹配第一条命中"，
// 增量修改会让节点的规则顺序取决于历史操作顺序——而那正是"同样的规则在
// 两台机器上效果不同"的来源。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p aclParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vpcacl] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVpcACLApply,
		NodeID: *t.NodeID,
		Target: "vpc-acl",
		Params: map[string]any{"spec": p.Spec, "version": p.Version},
	})
	if err != nil {
		return api.Unavailable("节点不可达，ACL 未应用")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	return nil
}
