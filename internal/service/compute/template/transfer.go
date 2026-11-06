package template

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/task"
)

// --- 导出 ---

// ExportView 是一个导出产物的对外视图。
type ExportView struct {
	ID           int64  `json:"id"`
	NodeID       int64  `json:"node_id"`
	TemplateID   int64  `json:"template_id"`
	TemplateName string `json:"template_name"`
	Filename     string `json:"filename"`
	SizeBytes    int64  `json:"size_bytes"`
	// SHA256 是导出包的内容摘要（第 3 轮 DEF-R3-01）。
	//
	// 透出它的理由：导入前要能核对"这个包是不是我导出的那个"。摘要在节点
	// 打包时就算好了，控制面只是把它存下来并原样给出去——节点侧的校验逻辑
	// 一点没动。历史记录（这一列还没有时导出的）会是空串，界面据此显示"—"。
	SHA256     string `json:"sha256"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// Export 受理一次模板导出（F-3-05）。
//
// 导出的是**模板盘本身**，而不是某台虚拟机——因此源模板必须就绪。它走
// 队列：打包一块几十 GB 的镜像不可能在请求里同步完成。
func (s *Service) Export(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if tpl.Status != model.TemplateReady {
		return nil, api.ValidationFailed("模板尚未就绪，不能导出")
	}

	// 导出包同样占空间，因此与模板本身一样计入存储配额。
	if s.quota != nil {
		if err := s.quota.Check(ctx, v.UserID, tpl.NodeID,
			int64(tpl.DiskSizeGB)*1024*1024*1024); err != nil {
			return nil, err
		}
	}

	familyName := ""
	if tpl.FamilyID != nil {
		var root model.Template
		if err := s.db.WithContext(ctx).Select("id", "name").
			Where("id = ?", *tpl.FamilyID).First(&root).Error; err == nil {
			familyName = root.Name
		}
	}

	row := model.TemplateExport{
		NodeID: tpl.NodeID, TemplateID: tpl.ID, TemplateName: tpl.Name,
		RelPath: "", Filename: tpl.Name + ".tar.gz",
		Status:    model.TemplateExportPending,
		CreatedBy: &v.UserID,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[template] 创建导出记录失败 id=%d: %v", tpl.ID, err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskTemplateExport,
		NodeID:       tpl.NodeID,
		ResourceType: "template_export",
		ResourceID:   row.ID,
		ResourceName: tpl.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: templateExportParams{
			ExportID: row.ID, TemplateID: tpl.ID, TemplateName: tpl.Name,
			DiskPath: tpl.DiskPathOf(), DiskFormat: tpl.DiskFormat,
			DiskSizeGB: tpl.DiskSizeGB, Version: tpl.Version,
			FamilyName: familyName,
			// 清单要能回答"导入后是什么规格"；这些元数据只存在控制面，
			// 节点不认识模板记录，必须随下发带入。
			OSType: derefString(tpl.OSType), OSVariant: derefString(tpl.OSVariant),
			DefaultCPU: tpl.DefaultCPU, DefaultMemoryMB: tpl.DefaultMemoryMB,
			DefaultDiskBus:  derefString(tpl.DefaultDiskBus),
			DefaultNicModel: derefString(tpl.DefaultNicModel),
			DefaultFirmware: derefString(tpl.DefaultFirmware),
		},
	})
	if err != nil {
		// 入队失败就把记录标成失败：留一条 pending 会让用户一直等一个
		// 永远不会开始的导出。
		s.markExport(ctx, row.ID, model.TemplateExportFailed, "导出任务入队失败")
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.export", tpl.NodeID, tpl.Name, t.ID)
	return t, nil
}

// ListExports 列出某节点的导出产物。
func (s *Service) ListExports(ctx context.Context, nodeID int64, v authz.Viewer) ([]ExportView, error) {
	query := s.db.WithContext(ctx).Model(&model.TemplateExport{}).
		Order("created_at DESC")
	if nodeID > 0 {
		query = query.Where("node_id = ?", nodeID)
	}
	if !v.IsAdmin {
		// 租户只看自己发起的：导出包里是模板盘，而私有模板不该因为
		// 一次导出就变成别人可见的东西。
		query = query.Where("created_by = ?", v.UserID)
	}

	var rows []model.TemplateExport
	if err := query.Find(&rows).Error; err != nil {
		log.Printf("[template] 查询导出记录失败: %v", err)
		return nil, api.Internal()
	}
	out := make([]ExportView, 0, len(rows))
	for i := range rows {
		out = append(out, toExportView(&rows[i]))
	}
	return out, nil
}

// ExportFile 取回一个导出产物的字节。
//
// 转发而不是给一个节点直链：直链意味着要把节点的访问凭据或一个匿名可访问
// 的地址暴露出去，而包里是一块模板盘。控制面转发多花一次带宽，但权限判断
// 留在了一处。
func (s *Service) ExportFile(
	ctx context.Context, id int64, v authz.Viewer,
) (fileName string, reader io.Reader, mime string, err error) {
	row, err := s.loadExport(ctx, id, v)
	if err != nil {
		return "", nil, "", err
	}
	if row.Status != model.TemplateExportSuccess {
		return "", nil, "", api.ValidationFailed("该导出尚未完成，暂时无法下载")
	}
	if row.RelPath == "" {
		return "", nil, "", api.NotFound("该导出的产物已不在节点上")
	}

	// 与虚拟机导出同一条路：模板包可能几十 GB，整包塞进一次 gRPC
	// 消息会被默认 4MB / 显式 32MB 的收包上限拦下，控制面也会整段
	// 载入内存。分片拉取、HTTP 层流式写出。
	const chunk = 8 << 20
	first, total, err := s.fetchExportChunk(ctx, row, 0, chunk)
	if err != nil {
		return "", nil, "", err
	}
	if len(first) == 0 {
		return "", nil, "", api.Unavailable("节点未返回导出包内容")
	}
	mimeType := "application/gzip"
	return row.Filename, newTemplateChunkedReader(s, row, first, total, chunk), mimeType, nil
}

// fetchExportChunk 取模板包的一段，返回 (数据, 文件总长)。
func (s *Service) fetchExportChunk(
	ctx context.Context, row *model.TemplateExport, offset, length int64,
) ([]byte, int64, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpTemplateExportFetch,
		NodeID: row.NodeID,
		Target: row.RelPath,
		Params: map[string]any{
			"rel_path": row.RelPath,
			"offset":   float64(offset),
			"length":   float64(length),
		},
	})
	if err != nil {
		return nil, 0, api.Unavailable("节点不可达，无法获取导出包")
	}
	if !result.Success {
		return nil, 0, api.ValidationFailed(result.Message)
	}
	content := decodeExportContent(result.Data)
	total := int64(0)
	if t, ok := result.Data["total_size"].(float64); ok {
		total = int64(t)
	}
	return content.Data, total, nil
}

// templateChunkedReader 把分片拉取拼成 io.Reader。
type templateChunkedReader struct {
	s        *Service
	row      *model.TemplateExport
	buf      []byte
	off      int
	next     int64
	total    int64
	chunkLen int64
}

func newTemplateChunkedReader(
	s *Service, row *model.TemplateExport, first []byte, total, chunk int64,
) *templateChunkedReader {
	return &templateChunkedReader{
		s: s, row: row, buf: first, total: total,
		chunkLen: chunk, next: int64(len(first)),
	}
}

func (r *templateChunkedReader) Read(p []byte) (int, error) {
	for r.off >= len(r.buf) {
		// 节点没报 total（旧版 agent）时按「首片即全部」结束；真实大包
		// 必须由新版 agent 提供总长，否则无法判断终点。
		if r.total <= 0 || r.next >= r.total {
			return 0, io.EOF
		}
		data, total, err := r.s.fetchExportChunk(context.Background(), r.row, r.next, r.chunkLen)
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			return 0, io.EOF
		}
		r.buf, r.off, r.total = data, 0, total
		r.next += int64(len(data))
	}
	n := copy(p, r.buf[r.off:])
	r.off += n
	return n, nil
}

// loadExport 读取一条导出记录并做归属校验。
func (s *Service) loadExport(ctx context.Context, id int64, v authz.Viewer) (*model.TemplateExport, error) {
	var row model.TemplateExport
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("导出记录不存在")
		}
		log.Printf("[template] 查询导出记录失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}
	if !v.IsAdmin && (row.CreatedBy == nil || *row.CreatedBy != v.UserID) {
		return nil, api.NotFound("导出记录不存在")
	}
	return &row, nil
}

// decodeExportContent 从结果里取出字节内容。
func decodeExportContent(data map[string]any) agent.ExportContent {
	raw, ok := data[agent.ExportContentKey]
	if !ok {
		return agent.ExportContent{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.ExportContent{}
	}
	var content agent.ExportContent
	_ = json.Unmarshal(blob, &content)
	return content
}

// DeleteExport 删除一个导出产物。
func (s *Service) DeleteExport(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	// 删除只要求"存在且属于自己"：未完成的导出也要能清掉——那通常是上一次
	// 失败的残留。用 loadExport 而不是 ExportFile，后者会要求产物已就绪
	// 并去节点取字节，而删一条残留不需要那些。
	row, err := s.loadExport(ctx, id, v)
	if err != nil {
		return nil, err
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskTemplateExportDelete,
		NodeID:       row.NodeID,
		ResourceType: "template_export",
		ResourceID:   row.ID,
		ResourceName: row.Filename,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: templateExportDeleteParams{
			ExportID: row.ID, RelPath: row.RelPath,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.export.delete", row.NodeID, row.Filename, t.ID)
	return t, nil
}

// --- 导入 ---

// ImportPreview 预览一个模板包（F-3-05）。
//
// 先验后做是这里唯一合理的顺序：包是别人给的，控制面在受理前必须先把
// "将导入成什么"显示给用户——名字撞了、格式不对、摘要不符，这三类问题
// 都应当在**导入之前**被看见。
func (s *Service) ImportPreview(
	ctx context.Context, req ImportRequest, v authz.Viewer,
) (*ImportPreviewView, error) {
	file, err := s.loadImportSource(ctx, req, v)
	if err != nil {
		return nil, err
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpTemplateImportPreview,
		NodeID: file.NodeID,
		Target: file.Filename,
		Params: map[string]any{"rel_path": file.RelPath},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取模板包")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info := decodeImportInfo(result.Data)

	view := &ImportPreviewView{
		Manifest:   manifestView(info.Manifest),
		CanImport:  !info.DigestMismatch,
		Message:    info.Message,
		SourceName: file.Filename,
	}
	if info.DigestMismatch {
		view.Reason = "包内容与清单里的摘要不符，可能已损坏或被改动"
		return view, nil
	}
	// 同名检查：导入之后名字必须唯一，而这个冲突**现在**就能判定。
	//
	// 与导入执行器共用 templateNameTaken（第 3 轮 DEF-R3-06）：两处各写一份
	// 查询的结局是"预览说能导、落库必然失败"——以前预览靠 GORM 自动过滤
	// 软删行，而唯一索引不排除软删行，两边对"同名"的定义不同。
	taken, err := templateNameTaken(s.db.WithContext(ctx), file.NodeID, info.Manifest.Name)
	if err != nil {
		log.Printf("[template] 检查模板重名失败: %v", err)
		return nil, api.Internal()
	}
	if taken {
		view.CanImport = false
		view.Reason = templateNameTakenMessage(info.Manifest.Name)
	}
	return view, nil
}

// templateNameTaken 报告该节点上是否已有**当前存在**的同名模板。
//
// **软删行不算占位**（与 vpc_switch 的 DEF-R3-12 同一口径）：模板删除是软删，
// 记录要留给审计与历史任务引用，但名字应该能被重新使用。
//
// 显式写 `deleted_at IS NULL` 而不是依赖 GORM 的自动过滤：自动过滤是"看不见
// 的行为"，一旦模型换成 `*time.Time`（本项目有多处）它就静默失效了——而这
// 里正是那种失效会变成"预览与实际相反"的地方。
//
// 同一个函数被**预览**与**导入执行器**共用，两处判定必须给出同一个答案。
func templateNameTaken(q *gorm.DB, nodeID int64, name string) (bool, error) {
	if name == "" {
		return false, nil
	}
	var dup int64
	if err := q.Model(&model.Template{}).
		Where("node_id = ? AND name = ? AND deleted_at IS NULL", nodeID, name).
		Count(&dup).Error; err != nil {
		return false, err
	}
	return dup > 0, nil
}

// templateNameTakenMessage 是同名冲突的**唯一**文案。
//
// 预览与执行器各写一句的后果是同一个冲突在界面上有两种说法，用户会以为是
// 两件不同的事。
func templateNameTakenMessage(name string) string {
	return "该节点上已有同名模板「" + name + "」，请先改名或删除现有的那个"
}

// strDeref 取可空字符串的值（nil → 空串）。
func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ImportRequest 是一次导入请求。
type ImportRequest struct {
	// FileID 指向「我的存储」里的模板包（category = template_package）。
	FileID int64
}

// ImportPreviewView 是导入前的预览结果。
type ImportPreviewView struct {
	SourceName string       `json:"source_name"`
	Manifest   ManifestView `json:"manifest"`
	CanImport  bool         `json:"can_import"`
	Reason     string       `json:"reason,omitempty"`
	Message    string       `json:"message,omitempty"`
}

// ManifestView 是模板包清单的对外视图。
type ManifestView struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	FamilyName string `json:"family_name,omitempty"`
	DiskFormat string `json:"disk_format"`
	DiskSizeGB int    `json:"disk_size_gb"`
	OSType     string `json:"os_type,omitempty"`
}

// Import 受理一次导入。
func (s *Service) Import(
	ctx context.Context, req ImportRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	// 受理前再验一次：预览与导入之间可能隔了几分钟，而这期间目标节点上
	// 完全可能出现一个同名模板。
	preview, err := s.ImportPreview(ctx, req, v)
	if err != nil {
		return nil, err
	}
	if !preview.CanImport {
		return nil, api.Conflict(preview.Reason)
	}

	file, err := s.loadImportSource(ctx, req, v)
	if err != nil {
		return nil, err
	}
	if s.quota != nil {
		if err := s.quota.Check(ctx, v.UserID, file.NodeID,
			int64(preview.Manifest.DiskSizeGB)*1024*1024*1024); err != nil {
			return nil, err
		}
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskTemplateImport,
		NodeID:       file.NodeID,
		ResourceType: "storage_file",
		ResourceID:   file.ID,
		ResourceName: file.Filename,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: templateImportParams{
			FileID: file.ID, RelPath: file.RelPath, Filename: file.Filename,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.import", file.NodeID, file.Filename, t.ID)
	return t, nil
}

// loadImportSource 取出并校验导入来源。
func (s *Service) loadImportSource(
	ctx context.Context, req ImportRequest, v authz.Viewer,
) (*model.StorageFile, error) {
	if req.FileID <= 0 {
		return nil, api.InvalidParameter("请选择要导入的模板包")
	}
	var file model.StorageFile
	err := s.db.WithContext(ctx).Where("id = ?", req.FileID).First(&file).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("文件不存在")
	case err != nil:
		log.Printf("[template] 查询导入来源失败: %v", err)
		return nil, api.Internal()
	}
	// 类别必须是模板包：一个 .qcow2 被当成模板包解，节点会在解包那一步
	// 失败，而那时用户已经等了几分钟。
	if file.Category != model.FileCategoryTemplatePackage {
		return nil, api.ValidationFailed("该文件不是模板包（类别为 " + file.Category + "）")
	}
	if !file.IsReady() {
		return nil, api.ValidationFailed("文件尚未上传完成")
	}
	if !v.IsAdmin && (file.UserID == nil || *file.UserID != v.UserID) {
		return nil, api.NotFound("文件不存在")
	}
	return &file, nil
}

// --- 内部辅助 ---

type templateExportParams struct {
	ExportID     int64  `json:"export_id"`
	TemplateID   int64  `json:"template_id"`
	TemplateName string `json:"template_name"`
	DiskPath     string `json:"disk_path"`
	DiskFormat   string `json:"disk_format"`
	DiskSizeGB   int    `json:"disk_size"`
	Version      int    `json:"version"`
	FamilyName   string `json:"family_name"`

	OSType          string `json:"os_type"`
	OSVariant       string `json:"os_variant"`
	DefaultCPU      int    `json:"default_cpu"`
	DefaultMemoryMB int    `json:"default_memory_mb"`
	DefaultDiskBus  string `json:"default_disk_bus"`
	DefaultNicModel string `json:"default_nic_model"`
	DefaultFirmware string `json:"default_firmware"`
}

type templateExportDeleteParams struct {
	ExportID int64  `json:"export_id"`
	RelPath  string `json:"rel_path"`
}

type templateImportParams struct {
	FileID   int64  `json:"file_id"`
	RelPath  string `json:"rel_path"`
	Filename string `json:"filename"`
}

// markExport 更新导出记录的状态。失败只记日志：它是一条展示用的记录。
func (s *Service) markExport(ctx context.Context, id int64, status, message string) {
	updates := map[string]any{"status": status, "finished_at": time.Now()}
	if message != "" {
		updates["error"] = message
	} else {
		updates["error"] = nil
	}
	if err := s.db.WithContext(ctx).Model(&model.TemplateExport{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[template] 更新导出记录失败 id=%d: %v", id, err)
	}
}

func toExportView(e *model.TemplateExport) ExportView {
	view := ExportView{
		ID: e.ID, NodeID: e.NodeID, TemplateID: e.TemplateID,
		TemplateName: e.TemplateName, Filename: e.Filename,
		SizeBytes: e.SizeBytes, Status: e.Status,
		SHA256:    strDeref(e.SHA256),
		CreatedAt: e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if e.Error != nil {
		view.Error = *e.Error
	}
	if e.FinishedAt != nil {
		view.FinishedAt = e.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

func manifestView(m agent.TemplateManifest) ManifestView {
	return ManifestView{
		Name: m.Name, Version: m.Version, FamilyName: m.FamilyName,
		DiskFormat: m.DiskFormat, DiskSizeGB: m.DiskSizeGB, OSType: m.OSType,
	}
}

// decodeImportInfo 从结果里取出导入信息。
//
// 走一遍 JSON：同进程是结构体、跨进程是 map，两种形状都要能读。
func decodeImportInfo(data map[string]any) agent.TemplateImportInfo {
	raw, ok := data[agent.TemplateImportDataKey]
	if !ok {
		return agent.TemplateImportInfo{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.TemplateImportInfo{}
	}
	var info agent.TemplateImportInfo
	_ = json.Unmarshal(blob, &info)
	return info
}

// derefString 把可空字符串转成空串（清单字段缺省即空）。
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
