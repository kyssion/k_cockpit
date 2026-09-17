package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// migrateParams 是 vm.migrate 任务的参数。
type migrateParams struct {
	MigrationID    int64  `json:"migration_id"`
	VMID           int64  `json:"vm_id"`
	VMName         string `json:"vm_name"`
	FromNodeID     int64  `json:"from_node_id"`
	ToNodeID       int64  `json:"to_node_id"`
	ObservedStatus string `json:"observed_status"`
}

// MigrateExecutor 执行 vm.migrate 任务（F-2-09）。
type MigrateExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewMigrateExecutor 构造迁移执行器。
func NewMigrateExecutor(db *gorm.DB, client agent.Client) *MigrateExecutor {
	return &MigrateExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *MigrateExecutor) Type() string { return model.TaskVMMigrate }

// Run 下发迁移指令并把虚拟机及其节点内资源搬到目标节点。
//
// **只在节点确认成功之后才改控制面记录**。反过来的话，迁移失败时控制面
// 会认为虚拟机已经在目标节点上，而它的磁盘其实还留在源节点——用户去目标
// 节点找不到它，去源节点又被告知它不在那儿。
//
// 失败时**不动任何记录**：源侧的磁盘与配置都还在原处，虚拟机在那里仍然
// 可用。迁移失败时用户至少还有一台能用的机器，这是本操作的底线。
func (e *MigrateExecutor) Run(ctx context.Context, t *model.Task) error {
	var p migrateParams
	if !decodeMigrate(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	e.markRunning(ctx, p.MigrationID)

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMMigrate,
		NodeID: p.FromNodeID,
		Target: p.VMName,
		Params: map[string]any{"to_node_id": p.ToNodeID},
	})
	if err != nil {
		e.markFailed(ctx, p.MigrationID, "源节点不可达，迁移指令未送达")
		return api.Unavailable("源节点不可达，迁移指令未送达")
	}
	if !result.Success {
		// 节点侧的失败原因更准（空间不足、磁盘读不出来），原样带出。
		e.markFailed(ctx, p.MigrationID, result.Message)
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.MigrateResultKey].(agent.MigrateResult)

	if err := e.commitMigration(ctx, &p, info); err != nil {
		// 节点侧已经搬完了，但控制面没跟上——这是最需要留痕的一种状态，
		// 因为对账程序要靠日志知道该往哪儿收敛。
		log.Printf("[vm] 迁移已由节点完成但控制面写入失败 vm=%d: %v", p.VMID, err)
		e.markFailed(ctx, p.MigrationID, "节点已完成迁移，但控制面记录更新失败，请联系管理员")
		return api.Internal()
	}

	log.Printf("[vm] 迁移完成 id=%d vm=%s %d → %d task=%d",
		p.MigrationID, p.VMName, p.FromNodeID, p.ToNodeID, t.ID)
	return nil
}

// commitMigration 在一个事务里把虚拟机与它的节点内资源搬到目标节点。
//
// **必须是一个事务**：把这些记录分开更新的话，中途失败会得到「虚拟机在
// 新节点上、而静态地址还指着旧节点」这种半迁移状态——那时两台机器上都
// 可能抢同一个 IP，而控制面认为一切正常。
func (e *MigrateExecutor) commitMigration(
	ctx context.Context, p *migrateParams, info agent.MigrateResult,
) error {
	now := time.Now()

	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) 虚拟机本身。
		if err := tx.Model(&model.VM{}).Where("id = ?", p.VMID).
			Updates(map[string]any{
				"node_id":        p.ToNodeID,
				"last_synced_at": now,
				// 跨节点之后投影里的一切都可能不同（IP、状态），因此把上次
				// 同步时间一并刷新，让界面不要拿旧节点的新鲜度糊弄用户。
				"status": model.VMStatusUnknown,
			}).Error; err != nil {
			return fmt.Errorf("更新虚拟机节点: %w", err)
		}

		// 2) 节点内资源跟随。**不搬它们的话**：静态地址仍然指向旧节点，
		// 而它在目标节点上没有被登记——于是「地址在节点内唯一」这条约束
		// 对新位置失效，另一台机器可以分配到同一个 IP。
		if err := tx.Model(&model.VMInterface{}).Where("vm_id = ?", p.VMID).
			Update("node_id", p.ToNodeID).Error; err != nil {
			return fmt.Errorf("更新网卡节点: %w", err)
		}
		if err := tx.Model(&model.StaticIP{}).Where("vm_id = ?", p.VMID).
			Update("node_id", p.ToNodeID).Error; err != nil {
			return fmt.Errorf("更新静态地址节点: %w", err)
		}
		if err := tx.Model(&model.PortForward{}).Where("vm_id = ?", p.VMID).
			Update("node_id", p.ToNodeID).Error; err != nil {
			return fmt.Errorf("更新端口转发节点: %w", err)
		}

		// 3) 迁移记录落终态。
		resultText := "已迁移"
		if len(info.Moved) > 0 {
			resultText = "已迁移：" + joinConflicts(info.Moved)
		}
		if info.DurationSeconds > 0 {
			resultText += fmt.Sprintf("（传输耗时 %d 秒）", info.DurationSeconds)
		}
		if len(resultText) > 500 {
			resultText = resultText[:500]
		}
		return tx.Model(&model.VMMigration{}).Where("id = ?", p.MigrationID).
			Updates(map[string]any{
				"status":      model.MigrationSuccess,
				"result":      resultText,
				"error":       nil,
				"finished_at": now,
			}).Error
	})
}

func (e *MigrateExecutor) markRunning(ctx context.Context, migrationID int64) {
	if err := e.db.WithContext(ctx).Model(&model.VMMigration{}).
		Where("id = ?", migrationID).Update("status", model.MigrationRunning).Error; err != nil {
		log.Printf("[vm] 更新迁移状态失败 id=%d: %v", migrationID, err)
	}
}

func (e *MigrateExecutor) markFailed(ctx context.Context, migrationID int64, message string) {
	if len(message) > 500 {
		message = message[:500]
	}
	if err := e.db.WithContext(ctx).Model(&model.VMMigration{}).
		Where("id = ?", migrationID).
		Updates(map[string]any{
			"status":      model.MigrationFailed,
			"error":       message,
			"finished_at": time.Now(),
		}).Error; err != nil {
		log.Printf("[vm] 标记迁移失败状态出错 id=%d: %v", migrationID, err)
	}
}

func decodeMigrate(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[vm] 解析迁移参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}
