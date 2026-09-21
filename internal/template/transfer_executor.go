package template

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

// ExportExecutor 打包一个模板。
type ExportExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExportExecutor 构造导出执行器。
func NewExportExecutor(db *gorm.DB, client agent.Client) *ExportExecutor {
	return &ExportExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ExportExecutor) Type() string { return model.TaskTemplateExport }

// Run 下发打包指令并写入产物记录。
//
// 顺序与制备一致：**先让节点打包成功、再写记录**。反过来的话，打包失败会
// 留下一条指向不存在文件的导出，用户下载时只会得到一个 404，而列表上写着
// "导出成功"。
func (e *ExportExecutor) Run(ctx context.Context, t *model.Task) error {
	var p templateExportParams
	if err := decodeTaskParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpTemplateExport,
		NodeID: *t.NodeID,
		Target: p.TemplateName,
		Params: map[string]any{
			"name":        p.TemplateName,
			"version":     p.Version,
			"family_name": p.FamilyName,
			"disk_path":   p.DiskPath,
			"disk_format": p.DiskFormat,
			"disk_size":   p.DiskSizeGB,
		},
	})
	if err != nil {
		e.finish(ctx, p.ExportID, model.TemplateExportFailed, "节点不可达，导出未执行")
		return api.Unavailable("节点不可达，导出未执行")
	}
	if !result.Success {
		e.finish(ctx, p.ExportID, model.TemplateExportFailed, result.Message)
		return api.ValidationFailed(result.Message)
	}

	info := decodeExportInfo(result.Data)
	relPath := info.RelPath
	if relPath == "" {
		// 节点没给路径就落失败：一条没有路径的"成功"记录，在下载时只会
		// 变成一次 404，而用户会以为文件丢了。
		e.finish(ctx, p.ExportID, model.TemplateExportFailed, "节点未返回导出包路径")
		return api.ValidationFailed("节点未返回导出包路径")
	}
	e.finish(ctx, p.ExportID, model.TemplateExportSuccess, "")

	filename := info.Filename
	if filename == "" {
		filename = pathBase(relPath)
	}
	if err := e.db.WithContext(ctx).Model(&model.TemplateExport{}).
		Where("id = ?", p.ExportID).
		Updates(map[string]any{
			"rel_path":   relPath,
			"filename":   filename,
			"size_bytes": info.SizeBytes,
		}).Error; err != nil {
		log.Printf("[template] 写入导出产物失败 id=%d: %v", p.ExportID, err)
	}
	return nil
}

// ExportDeleteExecutor 删除一个导出包。
type ExportDeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExportDeleteExecutor 构造删除执行器。
func NewExportDeleteExecutor(db *gorm.DB, client agent.Client) *ExportDeleteExecutor {
	return &ExportDeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ExportDeleteExecutor) Type() string { return model.TaskTemplateExportDelete }

// Run 下发删除指令。
//
// 记录**先删**：它指向的文件在节点上，而一次失败的下发不应该让一个已经被
// 删掉的包继续出现在列表里——那比"删了但文件还在"更容易误导人。
func (e *ExportDeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	var p templateExportDeleteParams
	if err := decodeTaskParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}
	if p.RelPath != "" {
		if _, err := e.agent.Execute(ctx, agent.Operation{
			Kind:   agent.OpTemplateExportDelete,
			NodeID: *t.NodeID,
			Target: p.RelPath,
			Params: map[string]any{"rel_path": p.RelPath},
		}); err != nil {
			// 节点不可达也删记录：留一条"删不掉"的死记录，用户会反复点。
			log.Printf("[template] 删除导出包失败 id=%d: %v", p.ExportID, err)
		}
	}
	if err := e.db.WithContext(ctx).
		Where("id = ?", p.ExportID).Delete(&model.TemplateExport{}).Error; err != nil {
		log.Printf("[template] 删除导出记录失败 id=%d: %v", p.ExportID, err)
	}
	return nil
}

// ImportExecutor 导入一个模板包。
type ImportExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewImportExecutor 构造导入执行器。
func NewImportExecutor(db *gorm.DB, client agent.Client) *ImportExecutor {
	return &ImportExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ImportExecutor) Type() string { return model.TaskTemplateImport }

// Run 下发导入指令并登记模板。
func (e *ImportExecutor) Run(ctx context.Context, t *model.Task) error {
	var p templateImportParams
	if err := decodeTaskParams(t, &p); err != nil {
		return err
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpTemplateImport,
		NodeID: *t.NodeID,
		Target: p.Filename,
		Params: map[string]any{"rel_path": p.RelPath},
	})
	if err != nil {
		return api.Unavailable("节点不可达，导入未执行")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	info := decodeImportInfo(result.Data)

	// 族：清单里的族名只在本节点已有同名模板时才接得上。
	//
	// 接不上就作为独立模板导入——凭空建一个"族"会让用户以为它和别处的
	// 某个模板同源，而实际上并没有。
	var familyID *int64
	if info.Manifest.FamilyName != "" {
		var root model.Template
		if err := e.db.WithContext(ctx).Select("id", "name").
			Where("node_id = ? AND name = ?", *t.NodeID, info.Manifest.FamilyName).
			First(&root).Error; err == nil {
			id := root.ID
			familyID = &id
		}
	}

	name := info.Manifest.Name
	if name == "" {
		name = "imported-" + time.Now().Format("20060102-150405")
	}
	version := info.Manifest.Version
	if version <= 0 {
		version = 1
	}

	tpl := model.Template{
		NodeID:          *t.NodeID,
		Name:            name,
		Version:         version,
		FamilyID:        familyID,
		Status:          model.TemplateReady,
		DiskFormat:      orDefault(info.Format, "qcow2"),
		DiskSizeGB:      pick(info.SizeGB, info.Manifest.DiskSizeGB),
		MinDiskGB:       pick(info.SizeGB, info.Manifest.DiskSizeGB),
		DefaultCPU:      info.Manifest.DefaultCPU,
		DefaultMemoryMB: info.Manifest.DefaultMemoryMB,
		Visibility:      model.TemplatePrivate,
		CloneEnabled:    true,
		CreatedBy:       t.CreatedBy,
	}
	if info.DiskPath != "" {
		tpl.DiskPath = &info.DiskPath
	}
	if m := info.Manifest; m.OSType != "" {
		osType := m.OSType
		tpl.OSType = &osType
	}
	if m := info.Manifest; m.OSVariant != "" {
		v := m.OSVariant
		tpl.OSVariant = &v
	}
	if info.Manifest.DefaultDiskBus != "" {
		v := info.Manifest.DefaultDiskBus
		tpl.DefaultDiskBus = &v
	}
	if info.Manifest.DefaultNicModel != "" {
		v := info.Manifest.DefaultNicModel
		tpl.DefaultNicModel = &v
	}
	if info.Manifest.DefaultFirmware != "" {
		v := info.Manifest.DefaultFirmware
		tpl.DefaultFirmware = &v
	}

	if err := e.db.WithContext(ctx).Create(&tpl).Error; err != nil {
		if isDuplicateKey(err) {
			return api.Conflict("该节点上已存在同名模板「" + name + "」")
		}
		log.Printf("[template] 写入导入的模板失败: %v", err)
		return api.Internal()
	}

	log.Printf("[template] 已导入模板 id=%d name=%s node=%d task=%d",
		tpl.ID, tpl.Name, tpl.NodeID, t.ID)
	return nil
}

// --- 辅助 ---

func (e *ExportExecutor) finish(ctx context.Context, id int64, status, message string) {
	updates := map[string]any{"status": status, "finished_at": time.Now()}
	if message != "" {
		updates["error"] = message
	} else {
		updates["error"] = nil
	}
	if err := e.db.WithContext(ctx).Model(&model.TemplateExport{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[template] 更新导出记录失败 id=%d: %v", id, err)
	}
}

func decodeTaskParams(t *model.Task, out any) error {
	if t.Params == nil {
		return api.Internal()
	}
	if err := json.Unmarshal([]byte(*t.Params), out); err != nil {
		log.Printf("[template] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	return nil
}

func decodeExportInfo(data map[string]any) agent.TemplateExportInfo {
	raw, ok := data[agent.TemplateExportDataKey]
	if !ok {
		return agent.TemplateExportInfo{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.TemplateExportInfo{}
	}
	var info agent.TemplateExportInfo
	_ = json.Unmarshal(blob, &info)
	return info
}

func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
