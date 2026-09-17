package securitygroup

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// applyParams 是 security_group.apply 任务的参数。
type applyParams struct {
	VMID    int64  `json:"vm_id"`
	VMName  string `json:"vm_name"`
	Version string `json:"version"`
	// Rules 是**已经汇总去重**的生效规则。
	//
	// 下发的是结果而不是「挂了哪几个组、各自的规则是什么」：汇总逻辑在
	// 控制面（它要读数据库），节点只负责把它变成宿主机的规则。让节点也做
	// 一遍汇总，两处的实现迟早会分叉，而分叉之后「界面上看到的」与
	// 「实际生效的」就不再是同一套东西了。
	Rules  []PrepareRule `json:"rules"`
	Groups []string      `json:"groups"`
}

// PrepareRule 是一条要下发的规则。
//
// 与对外视图分开定义：协议层不该依赖 handler 层的展示结构，那些结构会
// 随着界面需要而增删字段（比如补一个「来源组」），而节点根本不关心。
type PrepareRule struct {
	Direction     string `json:"direction"`
	Protocol      string `json:"protocol"`
	PortStart     *int   `json:"port_start,omitempty"`
	PortEnd       *int   `json:"port_end,omitempty"`
	TargetType    string `json:"target_type"`
	TargetValue   string `json:"target_value,omitempty"`
	AddressFamily string `json:"address_family"`
}

// ApplyExecutor 把生效规则下发到节点（F-4-04）。
type ApplyExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewApplyExecutor 构造执行器。
func NewApplyExecutor(db *gorm.DB, client agent.Client) *ApplyExecutor {
	return &ApplyExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ApplyExecutor) Type() string { return model.TaskSecurityGroupApply }

// Run 下发规则并记录已应用的版本。
//
// **只有下发成功才写 last_applied 版本**。反过来的话，节点失败时控制面
// 会认为规则已经生效——而用户按「已经生效」的前提去排查一条实际不存在的
// 放行规则，方向从一开始就是错的。
func (e *ApplyExecutor) Run(ctx context.Context, t *model.Task) error {
	var p applyParams
	if t.Params == nil {
		return api.Internal()
	}
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[securitygroup] 解析任务参数失败 task=%d: %v", t.ID, err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	rules := make([]map[string]any, 0, len(p.Rules))
	for _, r := range p.Rules {
		item := map[string]any{
			"direction": r.Direction, "protocol": r.Protocol,
			"target_type": r.TargetType, "address_family": r.AddressFamily,
		}
		if r.PortStart != nil {
			item["port_start"] = *r.PortStart
		}
		if r.PortEnd != nil {
			item["port_end"] = *r.PortEnd
		}
		if r.TargetValue != "" {
			item["target_value"] = r.TargetValue
		}
		rules = append(rules, item)
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpSecurityGroupApply,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			"vm_name": p.VMName,
			"version": p.Version,
			"groups":  p.Groups,
			"rules":   rules,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，规则未下发")
	}
	if !result.Success {
		// 节点侧的失败原因更准（规则冲突、链已存在），原样带出。
		return api.ValidationFailed(result.Message)
	}

	// 记录已应用的版本。它不是个纯粹的展示字段：
	//
	// 版本对不上说明「控制面认为生效的」与「宿主机上实际有的」不一致，
	// 而那种不一致是排查网络问题的第一个路口——先确认两边说的是不是
	// 同一件事，再去看规则内容。
	if err := e.db.WithContext(ctx).Model(&model.VMInterface{}).
		Where("vm_id = ?", p.VMID).
		Update("last_applied_at", t.UpdatedAt).Error; err != nil {
		log.Printf("[securitygroup] 回写应用时间失败 vm=%d: %v", p.VMID, err)
		// 规则已经下发成功，回写失败只影响展示——不因此把任务判为失败：
		// 那会让用户重试一次完整的下发，而规则本身没有任何问题。
	}

	log.Printf("[securitygroup] 规则已应用 vm=%s version=%s rules=%d",
		p.VMName, p.Version, len(p.Rules))
	return nil
}
