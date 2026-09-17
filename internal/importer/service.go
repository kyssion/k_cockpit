// Package importer 实现磁盘与镜像导入（F-2-13）。
//
// 包名用 importer 而不是 import：`import` 是 Go 关键字，不能做包名。
//
// 关于**上传**：本项目当前**不接收真实文件内容**。受理时前端只提交文件名、
// 大小与格式（都是浏览器能直接读到的元数据），然后由节点侧完成导入。
// 这样做让整条链路——受理、状态流转、格式转换后的建模板、配额记账——都能
// 被完整验证，而唯一没被验证的是「字节怎么从浏览器到宿主机」这一段。
//
// 那一段必须真实实现，且它是最难的部分之一（几十 GB 的分块、断点续传、
// 校验）。把它显式留成空缺，好过用一个假的传输层把接口形状定死——定错了
// 反而要改的地方更多。
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
	"k_cockpit/internal/vm"
)

// Service 提供导入能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
	quota vm.QuotaChecker
}

// NewService 构造导入服务。quota 允许为 nil。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client,
	recorder *audit.Recorder, quotaChecker vm.QuotaChecker,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder, quota: quotaChecker}
}

// View 是一次导入的对外视图。
type View struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`

	SourceFilename  string `json:"source_filename"`
	SourceFormat    string `json:"source_format"`
	SourceSizeBytes int64  `json:"source_size_bytes"`

	Status string `json:"status"`
	// TemplateID 是产出的模板——导入成功后才会有值。
	//
	// 界面据此给出「去克隆一台」的入口。**没有它的话，用户导入完会停在
	// 那个页面上不知道下一步做什么**：导入的产物是模板而不是虚拟机，
	// 而模板页在另一个地方。
	TemplateID *int64 `json:"template_id,omitempty"`
	Preview    any    `json:"preview,omitempty"`
	Error      string `json:"error,omitempty"`

	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// ParseRequest 是一次解析预览请求。
type ParseRequest struct {
	NodeID          int64
	SourceFilename  string
	SourceFormat    string
	SourceSizeBytes int64
}

// Parse 解析待导入的文件，返回配置预览（API-093 / f-2-13）。
//
// 对 **OVA**：包里带机器描述（OVF），由节点解析出 CPU、内存、磁盘等，
// 用户看到的是「这份镜像原本是怎么配的」。
//
// 对**裸镜像**：没有可解析的内容，因此预览里填的是用户将要使用的值——
// 界面据此让用户显式确认，而不是默默套一组默认值。
func (s *Service) Parse(
	ctx context.Context, req ParseRequest, v authz.Viewer,
) (*model.ImportPreview, error) {
	if err := validateSource(req.SourceFilename, req.SourceFormat); err != nil {
		return nil, err
	}
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}

	preview := &model.ImportPreview{Sources: map[string]string{}}

	if req.SourceFormat == model.ImportOVA {
		result, err := s.agent.Execute(ctx, agent.Operation{
			Kind:   agent.OpImageParse,
			NodeID: req.NodeID,
			Target: req.SourceFilename,
		})
		if err != nil {
			return nil, api.Unavailable("节点不可达，无法解析该包")
		}
		if !result.Success {
			return nil, api.ValidationFailed(result.Message)
		}
		parsed, ok := result.Data[agent.ImageParseDataKey].(agent.ImagePreview)
		if !ok {
			return nil, api.Unavailable("节点未能解析出机器描述")
		}
		preview = &model.ImportPreview{
			VCPU: parsed.VCPU, MemoryMB: parsed.MemoryMB, DiskGB: parsed.DiskGB,
			OSType: parsed.OSType, OSVariant: parsed.OSVariant,
			Sources: parsed.Sources,
		}
		if preview.Sources == nil {
			preview.Sources = map[string]string{}
		}
		// 每一项都标出来源：用户需要知道「这个 4 核是我自己选的，还是从
		// 包里读出来的」——两者对错误的含义完全不同。
		for _, k := range []string{"vcpu", "memory_mb", "disk_gb", "os_type"} {
			if _, ok := preview.Sources[k]; !ok {
				preview.Sources[k] = "ovf"
			}
		}
		preview.Notes = append(preview.Notes,
			"配置来自包内的 OVF 描述；磁盘格式会在导入时统一转换为 QCOW2。")
	} else {
		preview.Sources["vcpu"] = "user"
		preview.Sources["memory_mb"] = "user"
		preview.Sources["disk_gb"] = "user"
		preview.Notes = append(preview.Notes,
			"裸镜像不含机器描述，因此配置需要你自行指定；导入后可在编辑页调整。")
		if req.SourceFormat != model.ImportQCOW2 {
			preview.Notes = append(preview.Notes,
				"源格式为 "+model.ImportFormatLabel(req.SourceFormat)+
					"，导入时会自动转换为 QCOW2。")
		}
	}
	return preview, nil
}

// CreateRequest 是一次导入受理。
type CreateRequest struct {
	NodeID          int64
	Name            string
	SourceFilename  string
	SourceFormat    string
	SourceSizeBytes int64
	VCPU            int
	MemoryMB        int
	DiskGB          int
	OSType          string
	OSVariant       string
	Preview         *model.ImportPreview
}

// Create 受理一次导入（API-094）。
//
// 记录在**受理时**就创建（pending）：导入是长任务（格式转换可能处理几十 GB），
// 用户需要在那段时间里看到「有一个导入在进行」。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 64 {
		return nil, api.InvalidParameter("模板名称需为 1-64 个字符")
	}
	if err := validateSource(req.SourceFilename, req.SourceFormat); err != nil {
		return nil, err
	}
	if req.DiskGB <= 0 {
		return nil, api.InvalidParameter("必须指定磁盘大小")
	}
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}

	// 名字唯一性先查一次，给出一句能看懂的话。
	var dup int64
	if err := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("node_id = ? AND name = ?", req.NodeID, req.Name).
		Count(&dup).Error; err != nil {
		log.Printf("[import] 检查重名失败: %v", err)
		return nil, api.Internal()
	}
	if dup > 0 {
		return nil, api.Conflict("该节点上已有同名模板")
	}

	// 配额：导入的产物是模板，占用与原盘同级。用用户指定的大小估算——
	// 这比导出那个场景确定得多，因为大小是用户自己填的。
	if s.quota != nil {
		if err := s.quota.Check(ctx, v.UserID, req.NodeID,
			int64(req.DiskGB)*1024*1024*1024); err != nil {
			return nil, err
		}
	}

	previewJSON, _ := json.Marshal(req.Preview)
	previewStr := string(previewJSON)

	row := model.ImageImport{
		NodeID:          req.NodeID,
		Name:            req.Name,
		SourceFilename:  req.SourceFilename,
		SourceFormat:    req.SourceFormat,
		SourceSizeBytes: req.SourceSizeBytes,
		Status:          model.ImportPending,
		Preview:         &previewStr,
		CreatedBy:       &v.UserID,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[import] 创建导入记录失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskImageImport,
		NodeID:       req.NodeID,
		ResourceType: "node",
		ResourceID:   req.NodeID,
		ResourceName: req.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"import_id":         row.ID,
			"node_id":           req.NodeID,
			"name":              req.Name,
			"source_filename":   req.SourceFilename,
			"source_format":     req.SourceFormat,
			"source_size_bytes": req.SourceSizeBytes,
			"vcpu":              req.VCPU,
			"memory_mb":         req.MemoryMB,
			"disk_gb":           req.DiskGB,
			"os_type":           req.OSType,
			"os_variant":        req.OSVariant,
		},
	})
	if err != nil {
		// 入队失败要把记录清掉，否则会留下一条永远停在 pending 的导入。
		if delErr := s.db.WithContext(ctx).Delete(&model.ImageImport{}, row.ID).Error; delErr != nil {
			log.Printf("[import] 回滚导入记录失败 id=%d: %v", row.ID, delErr)
		}
		return nil, err
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			NodeID: req.NodeID, ResourceType: "image_import", ResourceID: row.ID,
			ResourceName: req.Name, Action: "image.import",
			Params: map[string]any{
				"task_id": t.ID, "format": req.SourceFormat,
				"filename": req.SourceFilename, "size_bytes": req.SourceSizeBytes,
			},
			Success: true, ClientIP: clientIP,
		})
	}
	return t, nil
}

// List 返回导入记录。
func (s *Service) List(ctx context.Context, v authz.Viewer, nodeID int64) ([]View, error) {
	var rows []model.ImageImport
	query := s.db.WithContext(ctx).Order("id DESC")
	if nodeID > 0 {
		query = query.Where("node_id = ?", nodeID)
	}
	// 普通用户只看自己发起的；管理员看全部。
	if !v.IsAdmin {
		query = query.Where("created_by = ?", v.UserID)
	}
	if err := query.Find(&rows).Error; err != nil {
		log.Printf("[import] 查询导入记录失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]View, 0, len(rows))
	for i := range rows {
		out = append(out, toView(&rows[i]))
	}
	return out, nil
}

// Get 返回单条导入记录。
func (s *Service) Get(ctx context.Context, id int64, v authz.Viewer) (*View, error) {
	row, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	view := toView(row)
	return &view, nil
}

// Delete 删除导入记录。
//
// **只删记录，不删产出的模板**：模板是独立的东西，可能已经被克隆成多台
// 虚拟机。删记录时顺手删模板，会让那些虚拟机失去来源——而它们在磁盘上
// 依赖的可能正是这份模板。要让用户去模板页显式删除。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	row, err := s.load(ctx, id, v)
	if err != nil {
		return err
	}
	if row.Status == model.ImportRunning || row.Status == model.ImportPending {
		return api.Conflict("该导入仍在进行中，请等待完成后再删除记录")
	}

	if err := s.db.WithContext(ctx).Delete(&model.ImageImport{}, row.ID).Error; err != nil {
		log.Printf("[import] 删除导入记录失败: %v", err)
		return api.Internal()
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			NodeID: row.NodeID, ResourceType: "image_import", ResourceID: row.ID,
			ResourceName: row.Name, Action: "image.import.delete",
			Success: true, ClientIP: clientIP,
		})
	}
	return nil
}

// 支持的源格式。
var supportedFormats = map[string]bool{
	model.ImportQCOW2: true, model.ImportRaw: true, model.ImportVMDK: true,
	model.ImportVHD: true, model.ImportVHDX: true, model.ImportIMG: true,
	model.ImportOVA: true,
}

// FormatFromFilename 按扩展名推断格式。
//
// 由**后端**推断而不是信前端传来的值：扩展名是用户唯一会认真看的东西，
// 而 formats 字段可能因为前端版本旧或请求被改而失真。以文件名为主、
// 以显式声明为辅，两边不一致时报错而不是猜。
func FormatFromFilename(name string) (string, bool) {
	idx := strings.LastIndex(name, ".")
	if idx < 0 || idx == len(name)-1 {
		return "", false
	}
	ext := strings.ToLower(name[idx+1:])
	if !supportedFormats[ext] {
		return "", false
	}
	return ext, true
}

func validateSource(filename, format string) error {
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return api.InvalidParameter("必须指定源文件名")
	}
	if !supportedFormats[format] {
		return api.InvalidParameter("不支持的源格式：" + format +
			"；支持 qcow2 / raw / vmdk / vhd / vhdx / img / ova")
	}
	// 扩展名与声明的格式必须一致。不一致多半意味着前端传错了值，
	// 而按其中任一个去执行都可能做错事——报出来让人查。
	byExt, ok := FormatFromFilename(filename)
	if !ok {
		return api.InvalidParameter("无法从文件名识别格式，请确认扩展名（qcow2 / raw / vmdk / vhd / vhdx / img / ova）")
	}
	if byExt != format {
		return api.InvalidParameter(
			"文件名扩展名（" + byExt + "）与声明的格式（" + format + "）不一致")
	}
	return nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	if nodeID <= 0 {
		return api.InvalidParameter("必须指定节点")
	}
	var n model.Node
	err := s.db.WithContext(ctx).Select("id", "maintenance_mode").
		Where("id = ?", nodeID).First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[import] 查询节点失败: %v", err)
		return api.Internal()
	}
	if n.MaintenanceMode {
		return api.ValidationFailed("节点处于维护模式，已暂停创建与导入")
	}
	return nil
}

func (s *Service) load(ctx context.Context, id int64, v authz.Viewer) (*model.ImageImport, error) {
	var row model.ImageImport
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("导入记录不存在")
	case err != nil:
		log.Printf("[import] 查询导入记录失败: %v", err)
		return nil, api.Internal()
	}
	// 用 404 而非 403：403 会确认「这个 ID 存在」，让 tenant 能通过枚举
	// 推断出别人导过多少东西。
	if !v.IsAdmin && (row.CreatedBy == nil || *row.CreatedBy != v.UserID) {
		return nil, api.NotFound("导入记录不存在")
	}
	return &row, nil
}

func toView(row *model.ImageImport) View {
	view := View{
		ID: row.ID, NodeID: row.NodeID, Name: row.Name,
		SourceFilename: row.SourceFilename, SourceFormat: row.SourceFormat,
		SourceSizeBytes: row.SourceSizeBytes, Status: row.Status,
		TemplateID: row.TemplateID,
		CreatedAt:  row.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if row.Error != nil {
		view.Error = *row.Error
	}
	if row.FinishedAt != nil {
		view.FinishedAt = row.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if row.Preview != nil && *row.Preview != "" {
		var p any
		if err := json.Unmarshal([]byte(*row.Preview), &p); err == nil {
			view.Preview = p
		}
	}
	return view
}
