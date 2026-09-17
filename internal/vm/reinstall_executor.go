package vm

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// reinstallParams 是 vm.reinstall 任务的参数。
type reinstallParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// TemplateDiskPath 随任务持久化，不在执行时回查模板表：任务可能排很久
	// 才执行，那时模板已被删除或改名，回查会得到空值或另一份路径。
	// **入队那一刻的路径才是这次要用的。**
	TemplateDiskPath string `json:"template_disk_path"`
	TemplateID       int64  `json:"template_id"`
	DiskGB           int    `json:"disk_gb"`
	ObservedStatus   string `json:"observed_status"`
}

// ReinstallExecutor 执行 vm.reinstall 任务（F-2-11）。
type ReinstallExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewReinstallExecutor 构造重装执行器。
func NewReinstallExecutor(db *gorm.DB, client agent.Client) *ReinstallExecutor {
	return &ReinstallExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (r *ReinstallExecutor) Type() string { return model.TaskVMReinstall }

// Run 下发重装指令并更新记录。
//
// 顺序：**先让节点重建成功、再更新记录**。反过来的话，重建失败时记录会显示
// 新模板与新系统盘，而实际磁盘还是旧的——用户按新系统的预期去操作一台旧
// 系统的机器，那是最难排查的一类错位。
//
// 失败时**不动记录里的备份路径**：节点侧的还原由它自己完成，而控制面保留
// 那条备份记录，等于给用户留了一个明确的「还有一份备份在」的信号。
func (r *ReinstallExecutor) Run(ctx context.Context, t *model.Task) error {
	var p reinstallParams
	if !decodeReinstall(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, r.agent, agent.Operation{
		Kind:   agent.OpVMReinstall,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			"template_id":        p.TemplateID,
			"template_disk_path": p.TemplateDiskPath,
			"disk_gb":            p.DiskGB,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，重装指令未送达")
	}
	if !result.Success {
		// 节点侧的失败原因更准（磁盘空间、模板可用性），原样带出。
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.ReinstallDataKey].(agent.ReinstallInfo)

	updates := map[string]any{
		"template_id":  p.TemplateID,
		"disk_gb":      p.DiskGB,
		"reinstall_at": time.Now(),
		// 新系统盘是干净的，状态大概率已变；以 agent 返回为准，拿不到时
		// 记 unknown 而**不是**猜一个 running——猜错会让界面显示「运行中」，
		// 而用户点关机才发现它没起来。
		"status":         statusOr(result, model.VMStatusUnknown),
		"last_synced_at": time.Now(),
	}
	if info.BackupPath != "" {
		updates["reinstall_backup"] = info.BackupPath
	}
	// 旧系统盘已随备份一起被替换，因此旧的依赖关系不再成立，按节点返回的
	// 结果重写。节点没说有没有父盘时记成 full——**宁可少报一个依赖**：
	// 多报会让本该能删的模板删不掉，而少报只影响一条提示信息。
	if info.BackingPath != "" {
		updates["clone_mode"] = model.CloneLinked
		updates["backing_path"] = info.BackingPath
	} else {
		updates["clone_mode"] = model.CloneFull
		updates["backing_path"] = nil
	}

	if err := r.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).Updates(updates).Error; err != nil {
		log.Printf("[vm] 更新重装结果失败 vm=%d: %v", p.VMID, err)
		// 不返回错误：宿主机上的重装已经完成，此时报失败会让用户以为没生效
		// 而去重试，而重试会被「有未清理的备份」拒绝，更困惑。
	}

	log.Printf("[vm] 重装完成 id=%d name=%s template=%d backup=%s task=%d",
		p.VMID, p.VMName, p.TemplateID, info.BackupPath, t.ID)
	return nil
}

// purgeParams 是 vm.reinstall.purge 任务的参数。
type purgeParams struct {
	VMID       int64  `json:"vm_id"`
	VMName     string `json:"vm_name"`
	BackupPath string `json:"backup_path"`
}

// PurgeExecutor 执行 vm.reinstall.purge 任务。
type PurgeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPurgeExecutor 构造备份清理执行器。
func NewPurgeExecutor(db *gorm.DB, client agent.Client) *PurgeExecutor {
	return &PurgeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (p *PurgeExecutor) Type() string { return model.TaskVMReinstallPurge }

// Run 下发清理指令并清空备份记录。
func (p *PurgeExecutor) Run(ctx context.Context, t *model.Task) error {
	var params purgeParams
	if !decodeReinstall(t, &params) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, p.agent, agent.Operation{
		Kind:   agent.OpVMReinstallPurge,
		NodeID: *t.NodeID,
		Target: params.VMName,
		Params: map[string]any{"backup_path": params.BackupPath},
	})
	if err != nil {
		return api.Unavailable("节点不可达，清理指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	if err := p.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", params.VMID).
		Updates(map[string]any{"reinstall_backup": nil}).Error; err != nil {
		log.Printf("[vm] 清空备份记录失败 vm=%d: %v", params.VMID, err)
	}

	log.Printf("[vm] 已清理重装备份 id=%d name=%s task=%d", params.VMID, params.VMName, t.ID)
	return nil
}

// statusOr 取节点返回的状态；未返回时用 fallback。
func statusOr(result *agent.Result, fallback string) string {
	if s, ok := result.Data[agent.StatusDataKey].(string); ok && s != "" {
		return s
	}
	return fallback
}

// decodeReinstall 解析任务参数。
func decodeReinstall(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[vm] 解析重装参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}
