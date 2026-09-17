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

// exportParams 是 vm.export 任务的参数。
type exportParams struct {
	ExportID         int64  `json:"export_id"`
	VMID             int64  `json:"vm_id"`
	VMName           string `json:"vm_name"`
	Format           string `json:"format"`
	IncludeDataDisks bool   `json:"include_data_disks"`
	ObservedStatus   string `json:"observed_status"`
}

// ExportExecutor 执行 vm.export 任务（F-2-14）。
type ExportExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExportExecutor 构造导出执行器。
func NewExportExecutor(db *gorm.DB, client agent.Client) *ExportExecutor {
	return &ExportExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ExportExecutor) Type() string { return model.TaskVMExport }

// Run 下发导出指令并回写产物信息。
//
// 与模板制备不同，导出记录**在受理时就已创建**（状态 pending）——导出可能
// 跑几十分钟，用户需要在那段时间里看到「有一个导出在进行」。因此这里的职责
// 是把那条记录**推进到终态**，而不是创建它。
//
// 失败时同样要落状态：记录停在 pending 会让界面一直显示「导出中」，
// 而任务早已失败——用户会一直等一个不会来的结果。
func (e *ExportExecutor) Run(ctx context.Context, t *model.Task) error {
	var p exportParams
	if !decodeExport(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	// 先标成 running：导出是长任务，pending 与 running 对用户的意义不同
	// （前者是「排队中」，后者是「正在打包」）。
	e.markRunning(ctx, p.ExportID)

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMExport,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			"format":             p.Format,
			"include_data_disks": p.IncludeDataDisks,
		},
	})
	if err != nil {
		e.markFailed(ctx, p.ExportID, "节点不可达，导出指令未送达")
		return api.Unavailable("节点不可达，导出指令未送达")
	}
	if !result.Success {
		e.markFailed(ctx, p.ExportID, result.Message)
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.ExportDataKey].(agent.ExportInfo)

	now := time.Now()
	updates := map[string]any{
		"status":      model.ExportSuccess,
		"finished_at": now,
		"error":       nil,
	}
	if info.FilePath != "" {
		updates["file_path"] = info.FilePath
	}
	if info.FileName != "" {
		updates["file_name"] = info.FileName
	} else {
		// 节点没给文件名时按「虚拟机名.格式」兜底，而不是留空——
		// 留空会让下载接口与界面都少一个可用的名字。
		updates["file_name"] = p.VMName + "." + p.Format
	}
	// 大小如实记录：它**计入用户的存储配额**（f-2-14），记 0 等于这份占用
	// 永远不算数。
	updates["size_bytes"] = info.SizeBytes

	if err := e.db.WithContext(ctx).Model(&model.VMExport{}).
		Where("id = ?", p.ExportID).Updates(updates).Error; err != nil {
		log.Printf("[vm] 回写导出结果失败 id=%d: %v", p.ExportID, err)
	}

	log.Printf("[vm] 导出完成 id=%d vm=%s format=%s size=%d task=%d",
		p.ExportID, p.VMName, p.Format, info.SizeBytes, t.ID)
	return nil
}

func (e *ExportExecutor) markRunning(ctx context.Context, exportID int64) {
	if err := e.db.WithContext(ctx).Model(&model.VMExport{}).
		Where("id = ?", exportID).Update("status", model.ExportRunning).Error; err != nil {
		log.Printf("[vm] 更新导出状态失败 id=%d: %v", exportID, err)
	}
}

func (e *ExportExecutor) markFailed(ctx context.Context, exportID int64, message string) {
	if len(message) > 500 {
		message = message[:500]
	}
	if err := e.db.WithContext(ctx).Model(&model.VMExport{}).
		Where("id = ?", exportID).
		Updates(map[string]any{
			"status":      model.ExportFailed,
			"error":       message,
			"finished_at": time.Now(),
		}).Error; err != nil {
		log.Printf("[vm] 标记导出失败状态出错 id=%d: %v", exportID, err)
	}
}

// exportDeleteParams 是 vm.export.delete 任务的参数。
type exportDeleteParams struct {
	ExportID int64  `json:"export_id"`
	FilePath string `json:"file_path"`
}

// ExportDeleteExecutor 执行 vm.export.delete 任务。
type ExportDeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExportDeleteExecutor 构造导出删除执行器。
func NewExportDeleteExecutor(db *gorm.DB, client agent.Client) *ExportDeleteExecutor {
	return &ExportDeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ExportDeleteExecutor) Type() string { return model.TaskVMExportDelete }

// Run 下发删除指令并软删记录。
func (e *ExportDeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	var p exportDeleteParams
	if !decodeExport(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMExportDelete,
		NodeID: *t.NodeID,
		Params: map[string]any{"file_path": p.FilePath},
	})
	if err != nil {
		return api.Unavailable("节点不可达，删除指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 软删除：产物占了配额，删掉之后**核销记录仍然要留**，供审计与事后
	// 对账。物理删除会让「这个配额是怎么算出来的」变成一笔无头账。
	if err := e.db.WithContext(ctx).Delete(&model.VMExport{}, p.ExportID).Error; err != nil {
		log.Printf("[vm] 标记导出产物已删除失败 id=%d: %v", p.ExportID, err)
	}

	log.Printf("[vm] 已删除导出产物 id=%d task=%d", p.ExportID, t.ID)
	return nil
}

// decodeExport 解析任务参数。
func decodeExport(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[vm] 解析导出参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}
