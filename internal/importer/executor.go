package importer

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// importParams 是 image.import 任务的参数。
type importParams struct {
	ImportID int64  `json:"import_id"`
	NodeID   int64  `json:"node_id"`
	Name     string `json:"name"`

	SourceFilename  string `json:"source_filename"`
	SourceFormat    string `json:"source_format"`
	SourceSizeBytes int64  `json:"source_size_bytes"`

	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	OSType    string `json:"os_type"`
	OSVariant string `json:"os_variant"`
}

// ImportExecutor 执行 image.import 任务（F-2-13）。
type ImportExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewImportExecutor 构造导入执行器。
func NewImportExecutor(db *gorm.DB, client agent.Client) *ImportExecutor {
	return &ImportExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ImportExecutor) Type() string { return model.TaskImageImport }

// Run 下发导入指令、产出模板并回写记录。
//
// 顺序：**先让节点转换成功、再建模板**。反过来的话，转换失败时会留下一个
// 指向不存在磁盘的模板，用户拿它去克隆，然后在某个说不清的时刻失败。
//
// 失败路径同样要推进记录状态：停在 pending 会让界面一直显示「导入中」，
// 而任务早已失败——用户会一直等一个不会来的结果。
func (e *ImportExecutor) Run(ctx context.Context, t *model.Task) error {
	var p importParams
	if !decode(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	e.markRunning(ctx, p.ImportID)

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpImageImport,
		NodeID: *t.NodeID,
		Target: p.SourceFilename,
		Params: map[string]any{
			"name":          p.Name,
			"source_format": p.SourceFormat,
			"size_bytes":    p.SourceSizeBytes,
		},
	})
	if err != nil {
		e.markFailed(ctx, p.ImportID, "节点不可达，导入指令未送达")
		return api.Unavailable("节点不可达，导入指令未送达")
	}
	if !result.Success {
		e.markFailed(ctx, p.ImportID, result.Message)
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.ImageImportDataKey].(agent.ImageImportInfo)

	// 产出模板——导入的产物是模板而不是虚拟机（见 model.ImageImport 的说明）。
	tpl := model.Template{
		NodeID: p.NodeID, Name: p.Name,
		Status:          model.TemplateReady,
		DiskFormat:      orDefault(info.Format, "qcow2"),
		DiskSizeGB:      pick(info.SizeGB, p.DiskGB),
		MinDiskGB:       p.DiskGB,
		DefaultCPU:      p.VCPU,
		DefaultMemoryMB: p.MemoryMB,
		Visibility:      model.TemplatePrivate,
		CloneEnabled:    true,
		CreatedBy:       t.CreatedBy,
	}
	if info.DiskPath != "" {
		tpl.DiskPath = &info.DiskPath
	}
	if p.OSType != "" {
		tpl.OSType = &p.OSType
	}
	if p.OSVariant != "" {
		tpl.OSVariant = &p.OSVariant
	}
	note := "由导入生成"
	if len(info.Notes) > 0 {
		note += "：" + strings.Join(info.Notes, "；")
	}
	tpl.Remark = &note

	if err := e.db.WithContext(ctx).Create(&tpl).Error; err != nil {
		if isDuplicateKey(err) {
			e.markFailed(ctx, p.ImportID, "同名模板已存在")
			return api.Conflict("同名模板已存在")
		}
		log.Printf("[import] 写入模板失败: %v", err)
		e.markFailed(ctx, p.ImportID, "写入模板记录失败")
		return api.Internal()
	}

	now := time.Now()
	updates := map[string]any{
		"status":      model.ImportSuccess,
		"finished_at": now,
		"error":       nil,
		"template_id": tpl.ID,
	}
	if info.DiskPath != "" {
		updates["disk_path"] = info.DiskPath
	}
	if err := e.db.WithContext(ctx).Model(&model.ImageImport{}).
		Where("id = ?", p.ImportID).Updates(updates).Error; err != nil {
		log.Printf("[import] 回写导入结果失败 id=%d: %v", p.ImportID, err)
	}

	log.Printf("[import] 导入完成 id=%d name=%s template=%d task=%d",
		p.ImportID, p.Name, tpl.ID, t.ID)
	return nil
}

func (e *ImportExecutor) markRunning(ctx context.Context, importID int64) {
	if err := e.db.WithContext(ctx).Model(&model.ImageImport{}).
		Where("id = ?", importID).Update("status", model.ImportRunning).Error; err != nil {
		log.Printf("[import] 更新导入状态失败 id=%d: %v", importID, err)
	}
}

func (e *ImportExecutor) markFailed(ctx context.Context, importID int64, message string) {
	if len(message) > 500 {
		message = message[:500]
	}
	if err := e.db.WithContext(ctx).Model(&model.ImageImport{}).
		Where("id = ?", importID).
		Updates(map[string]any{
			"status":      model.ImportFailed,
			"error":       message,
			"finished_at": time.Now(),
		}).Error; err != nil {
		log.Printf("[import] 标记导入失败状态出错 id=%d: %v", importID, err)
	}
}

func decode(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[import] 解析导入参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func pick(actual, fallback int) int {
	if actual > 0 {
		return actual
	}
	return fallback
}
