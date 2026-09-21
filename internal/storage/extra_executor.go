package storage

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// PartitionExecutor 执行分区的创建与删除。
//
// 分区**没有控制面记录**：分区表在磁盘上，控制面保存一份就等于承诺"我
// 知道现在有哪些分区"，而那在手工改过之后必然是错的。因此这里只下发，
// 需要看的时候再读（storage.partition.list）。
type PartitionExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPartitionExecutor 构造分区执行器。
func NewPartitionExecutor(client agent.Client) *PartitionExecutor {
	return &PartitionExecutor{agent: client}
}

// Type 返回处理的任务类型。
func (e *PartitionExecutor) Type() string { return model.TaskStoragePartitionCreate }

// Run 下发创建指令。
func (e *PartitionExecutor) Run(ctx context.Context, t *model.Task) error {
	var p partitionParams
	if err := decodeParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}
	_, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpStoragePartitionCreate,
		NodeID: *t.NodeID,
		Target: p.DeviceID,
		Params: map[string]any{"device_id": p.DeviceID, "size_gb": p.SizeGB},
	})
	if err != nil {
		return api.Unavailable("节点不可达，分区未创建")
	}
	return nil
}

// PartitionDeleteExecutor 执行分区删除。
type PartitionDeleteExecutor struct {
	agent agent.Client
}

// NewPartitionDeleteExecutor 构造删除执行器。
func NewPartitionDeleteExecutor(client agent.Client) *PartitionDeleteExecutor {
	return &PartitionDeleteExecutor{agent: client}
}

// Type 返回处理的任务类型。
func (e *PartitionDeleteExecutor) Type() string { return model.TaskStoragePartitionDelete }

// Run 下发删除指令。
func (e *PartitionDeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	var p partitionParams
	if err := decodeParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}
	_, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpStoragePartitionDelete,
		NodeID: *t.NodeID,
		Target: p.DeviceID,
		Params: map[string]any{"device_id": p.DeviceID, "index": p.Index, "all": p.All},
	})
	if err != nil {
		return api.Unavailable("节点不可达，分区未删除")
	}
	return nil
}

// PoolConfigExecutor 下发池配置。
type PoolConfigExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPoolConfigExecutor 构造池配置执行器。
func NewPoolConfigExecutor(db *gorm.DB, client agent.Client) *PoolConfigExecutor {
	return &PoolConfigExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PoolConfigExecutor) Type() string { return model.TaskStoragePoolConfig }

// Run 下发配置并把节点实际生效的结果写回。
//
// 写回**节点返回的挂载点**而不是请求里的值：节点可能按池名规范化了路径。
// 以它为准，否则面板上的挂载点会从这一刻开始与真实位置不一致。
func (e *PoolConfigExecutor) Run(ctx context.Context, t *model.Task) error {
	var p poolConfigParams
	if err := decodeParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}
	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpStoragePoolConfig,
		NodeID: *t.NodeID,
		Target: p.DeviceID,
		Params: specParams(p.Spec),
	})
	if err != nil {
		return api.Unavailable("节点不可达，池配置未下发")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	info := decodePoolConfig(result.Data)
	updates := map[string]any{"updated_at": time.Now()}
	if info.MountPath != "" {
		updates["mount_path"] = info.MountPath
	}
	if len(updates) > 1 {
		if err := e.db.WithContext(ctx).Model(&model.StoragePool{}).
			Where("id = ?", p.PoolID).Updates(updates).Error; err != nil {
			log.Printf("[storage] 写回池配置失败 id=%d: %v", p.PoolID, err)
		}
	}
	return nil
}

// PoolUnmountExecutor 卸载存储池。
type PoolUnmountExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPoolUnmountExecutor 构造卸载执行器。
func NewPoolUnmountExecutor(db *gorm.DB, client agent.Client) *PoolUnmountExecutor {
	return &PoolUnmountExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PoolUnmountExecutor) Type() string { return model.TaskStoragePoolUnmount }

// Run 下发卸载并更新状态。
func (e *PoolUnmountExecutor) Run(ctx context.Context, t *model.Task) error {
	var p poolConfigParams
	if err := decodeParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}
	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpStoragePoolUnmount,
		NodeID: *t.NodeID,
		Target: p.DeviceID,
		Params: map[string]any{"device_id": p.DeviceID},
	})
	if err != nil {
		return api.Unavailable("节点不可达，未卸载")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	// 卸载后记为已卸载：这既是状态展示，也是"下次别再自动挂"的依据。
	if err := e.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("id = ?", p.PoolID).
		Updates(map[string]any{
			"status":     model.StoragePoolUnmounted,
			"updated_at": time.Now(),
		}).Error; err != nil {
		log.Printf("[storage] 更新池状态失败 id=%d: %v", p.PoolID, err)
	}
	return nil
}

// --- 辅助 ---

func decodeParams(t *model.Task, out any) error {
	if t.Params == nil {
		return api.Internal()
	}
	if err := json.Unmarshal([]byte(*t.Params), out); err != nil {
		log.Printf("[storage] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	return nil
}

// specParams 把 spec 摊平到下发参数里。
//
// 保留 spec 嵌套会导致节点侧要区分"从哪一层取"，而这里只有一层——摊平
// 之后两边都只需要认一组键名。
func specParams(spec map[string]any) map[string]any {
	params := map[string]any{}
	for k, v := range spec {
		if strings.TrimSpace(k) == "" {
			continue
		}
		params[k] = v
	}
	return params
}
