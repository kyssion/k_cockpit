package vm

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// ConfigUpdateExecutor 执行 vm.config.update 任务（F-2-05 的硬件配置变更）。
type ConfigUpdateExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewConfigUpdateExecutor 构造配置变更执行器。
func NewConfigUpdateExecutor(db *gorm.DB, client agent.Client) *ConfigUpdateExecutor {
	return &ConfigUpdateExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ConfigUpdateExecutor) Type() string { return model.TaskVMConfigUpdate }

// Run 下发配置变更并回写投影。
func (e *ConfigUpdateExecutor) Run(ctx context.Context, t *model.Task) error {
	var p configChangeParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}
	if len(p.Changes) == 0 {
		// 受理时已经筛过空变更，这里再挡一次是为了防御历史任务：
		// 一条没有任何改动的任务执行下去只会白白触发一次节点往返。
		return api.InvalidParameter("没有需要修改的内容")
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMConfigUpdate,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{"changes": p.Changes},
	})
	if err != nil {
		return api.Unavailable("节点不可达，修改配置的指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// **只写变更过的字段**，不整行覆盖。
	//
	// 任务参数里只有差异集合（差异提交），整行覆盖会把没有提交的字段写成
	// 零值——一次「改内存」会顺手把 CPU 清成 0。这类问题在执行成功之后才
	// 显现，排查时很难联想到是回写逻辑的问题。
	updates := map[string]any{"last_synced_at": time.Now()}
	for key, value := range p.Changes {
		updates[key] = value
	}

	if err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).Updates(updates).Error; err != nil {
		// 宿主侧已改成功，只是本地投影没写上。不返回错误：让任务报失败会
		// 误导用户去重试一次已经生效的修改（重试还会因等值被受理层筛掉，
		// 表现为「点了没反应」，更困惑）。投影是缓存，下次对账会收敛。
		log.Printf("[vm] 回写配置变更失败 vm=%d: %v", p.VMID, err)
	}

	log.Printf("[vm] 配置变更完成 vm=%d changes=%v task=%d", p.VMID, p.Changes, t.ID)
	return nil
}
