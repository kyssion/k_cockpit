package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/importer"
	"k_cockpit/internal/model"
)

// Importer 提供磁盘与镜像导入接口（F-2-13）。
type Importer struct {
	svc *importer.Service
}

// NewImporter 构造导入接口。
func NewImporter(svc *importer.Service) *Importer {
	return &Importer{svc: svc}
}

// FormatFromFilename 按扩展名推断格式（API-093 的一部分）。
//
// 由**后端**推断而不是信前端传来的值：扩展名是用户唯一会认真看的东西。
// 前端拿到结果后填进下拉框——用户看到后端认出的格式，比看到自己选的更可信。
func (h *Importer) GuessFormat(ctx context.Context, c *app.RequestContext) {
	filename := c.Query("filename")
	format, ok := importer.FormatFromFilename(filename)
	if !ok {
		api.OK(c, map[string]any{"format": "", "supported": false})
		return
	}
	api.OK(c, map[string]any{
		"format":    format,
		"label":     model.ImportFormatLabel(format),
		"bundled":   format == model.ImportOVA,
		"supported": true,
	})
}

type parseRequest struct {
	NodeID          int64  `json:"node_id"`
	SourceFilename  string `json:"source_filename"`
	SourceFormat    string `json:"source_format"`
	SourceSizeBytes int64  `json:"source_size_bytes"`
}

// Parse 解析待导入的文件，返回配置预览（API-093）。
//
// **解析发生在受理之前**：f-2-13 要求「先解析预览再创建」，而预览的意义
// 就是让用户在按下确认之前看到将要创建的是什么。
func (h *Importer) Parse(ctx context.Context, c *app.RequestContext) {
	var req parseRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	preview, err := h.svc.Parse(ctx, importer.ParseRequest{
		NodeID: req.NodeID, SourceFilename: req.SourceFilename,
		SourceFormat: req.SourceFormat, SourceSizeBytes: req.SourceSizeBytes,
	}, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, preview)
}

type createImportRequest struct {
	NodeID          int64  `json:"node_id"`
	Name            string `json:"name"`
	SourceFilename  string `json:"source_filename"`
	SourceFormat    string `json:"source_format"`
	SourceSizeBytes int64  `json:"source_size_bytes"`

	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	OSType    string `json:"os_type"`
	OSVariant string `json:"os_variant"`
	Preview   any    `json:"preview"`
}

// Create 受理一次导入（API-094）。
func (h *Importer) Create(ctx context.Context, c *app.RequestContext) {
	var req createImportRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	preview := toPreview(req.Preview)

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Create(ctx, importer.CreateRequest{
		NodeID: req.NodeID, Name: req.Name,
		SourceFilename: req.SourceFilename, SourceFormat: req.SourceFormat,
		SourceSizeBytes: req.SourceSizeBytes,
		VCPU:            req.VCPU, MemoryMB: req.MemoryMB, DiskGB: req.DiskGB,
		OSType: req.OSType, OSVariant: req.OSVariant, Preview: preview,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// List 返回导入记录（API-095）。
func (h *Importer) List(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.List(ctx, authz.ViewerOf(c), int64(queryInt(c, "node_id")))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Get 返回单条导入记录（API-095）。
func (h *Importer) Get(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "导入 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	view, err := h.svc.Get(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Delete 删除导入记录（API-096）。
//
// **只删记录，不删产出的模板**：模板可能已经被克隆成多台虚拟机，顺手删掉
// 会让那些虚拟机失去来源，而它们在磁盘上依赖的可能正是这份模板。
func (h *Importer) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "导入 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.Delete(ctx, id, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

// toPreview 把请求里的原始 JSON 还原成结构化预览。
//
// 前端把预览**原样回传**：它是用户按下确认时看到的那一份，服务端留存下来
// 供事后回答「当初预览里写的是什么」。因此这里不做字段级校验——校验已经
// 在受理时按 vcpu / memory_mb 等具名字段做过了，重复一遍只会让两处慢慢分叉。
func toPreview(raw any) *model.ImportPreview {
	if raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	p := &model.ImportPreview{Sources: map[string]string{}}
	if v, ok := m["vcpu"].(float64); ok {
		p.VCPU = int(v)
	}
	if v, ok := m["memory_mb"].(float64); ok {
		p.MemoryMB = int(v)
	}
	if v, ok := m["disk_gb"].(float64); ok {
		p.DiskGB = int(v)
	}
	if v, ok := m["os_type"].(string); ok {
		p.OSType = v
	}
	if v, ok := m["os_variant"].(string); ok {
		p.OSVariant = v
	}
	if srcs, ok := m["sources"].(map[string]any); ok {
		for k, v := range srcs {
			if s, ok := v.(string); ok {
				p.Sources[k] = s
			}
		}
	}
	if notes, ok := m["notes"].([]any); ok {
		for _, n := range notes {
			if s, ok := n.(string); ok {
				p.Notes = append(p.Notes, s)
			}
		}
	}
	return p
}
