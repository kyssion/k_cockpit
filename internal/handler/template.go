package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/template"
)

// Template 提供模板接口（F-3-01 / F-3-02）。
//
// 权限：读接口对所有登录用户开放（可见性由服务层按「已发布 / 自己创建」
// 过滤），制备与删除要求使用者在服务层通过归属检查。
type Template struct {
	svc *template.Service
}

// NewTemplate 构造模板接口。
func NewTemplate(svc *template.Service) *Template {
	return &Template{svc: svc}
}

// List 返回可见的模板。
func (h *Template) List(ctx context.Context, c *app.RequestContext) {
	views, err := h.svc.List(ctx, template.ListFilter{
		NodeID:    int64(queryInt(c, "node_id")),
		Keyword:   c.Query("keyword"),
		OnlyReady: c.Query("only_ready") == "true",
		Viewer:    authz.ViewerOf(c),
	})
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, views)
}

// Get 返回模板详情。
func (h *Template) Get(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
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

type createTemplateRequest struct {
	VMID      int64  `json:"vm_id"`
	Name      string `json:"name"`
	OSType    string `json:"os_type"`
	OSVariant string `json:"os_variant"`
	Remark    string `json:"remark"`
	Published bool   `json:"published"`
	// ParentID 非空表示制备的是**某个模板的新版本**（F-3-04）。
	ParentID *int64 `json:"parent_id"`
}

// CreateFromVM 从一台虚拟机的系统盘制备模板（API-076）。
func (h *Template) CreateFromVM(ctx context.Context, c *app.RequestContext) {
	var req createTemplateRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: req.VMID, Name: req.Name, OSType: req.OSType,
		OSVariant: req.OSVariant, Remark: req.Remark, Published: req.Published,
		ParentID: req.ParentID,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

type updateTemplateRequest struct {
	Published    *bool  `json:"published"`
	Visibility   string `json:"visibility"`
	CloneEnabled *bool  `json:"clone_enabled"`
	Remark       string `json:"remark"`
}

// Update 修改模板（发布 / 停止提供克隆 / 备注）。
//
// **同步生效、不进任务队列**：这些都是纯控制面元数据，不影响宿主机上的
// 任何东西。做成任务会制造一个「界面说已发布、实际还没发布」的窗口，
// 而发布与否决定了别人能不能用——那是最不该含糊的一类状态。
func (h *Template) Update(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req updateTemplateRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Update(ctx, id, template.UpdateRequest{
		Published: req.Published, Visibility: req.Visibility,
		CloneEnabled: req.CloneEnabled, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Delete 删除模板。
//
// 仍有链式克隆依赖时**同步拒绝**并说明数量与出路——父盘一删，那些虚拟机的
// 数据就不可用了，而且不会立刻报错，要等到下次开机或读某个未缓存的数据块。
// DeletePreview 返回删除前的检查结果（API-033）。**只读**。
//
// 这些约束本来就有（Delete 里会拒绝），但用户只有在点了删除之后才会撞上
// ——而那时他看到的是一个错误提示，不是一份待办清单。预览把这件工作放在
// 「按下按钮之前」，并且给出**具体是哪几台**，而不只是一个计数。
func (h *Template) DeletePreview(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	view, err := h.svc.DeletePreview(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// deleteTemplateRequest 是删除请求。
//
// Strategy 只在**存在派生模板**时才有意义；链式克隆没有任何策略可以绕过
// （任何策略都意味着接受数据丢失）。
type deleteTemplateRequest struct {
	Strategy string `json:"strategy"`
}

func (h *Template) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req deleteTemplateRequest
	// 删除策略在查询串里也接受：DELETE 携带 body 并非所有客户端都支持，
	// 与虚拟机删除那里保持同一做法。
	_ = c.Bind(&req)
	if req.Strategy == "" {
		req.Strategy = c.Query("strategy")
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Delete(ctx, id, template.DeleteRequest{Strategy: req.Strategy},
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// --- 模板导出与导入（F-3-05）---

// Export 导出一个模板。
func (h *Template) Export(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Export(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// ListExports 列出导出产物。
func (h *Template) ListExports(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.ListExports(ctx, int64(queryInt(c, "node_id")), authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// DownloadExport 下载一个导出产物。
//
// 它走的是用户存储的下载通道：导出包本来就在**用户存储**里（这样它能被
// 当作导入来源选中、也受存储配额约束），因此不必为它另开一条读文件的路。
//
// 文件名由服务端决定而不是沿用请求里的名字：Content-Disposition 会被写进
// 响应头，让客户端指定文件名等于给它一个注入响应头的入口。
func (h *Template) DownloadExport(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "导出 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	name, data, mime, err := h.svc.ExportFile(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	// 产物一旦生成就不再变化，因此允许浏览器缓存（与控制台截帧相反）。
	c.Header("Cache-Control", "private, max-age=3600")
	// 文件名必须由服务端决定，且用 RFC 5987 的形式处理非 ASCII。
	c.Header("Content-Disposition", contentDisposition(name))
	c.SetContentType(mime)
	c.Response.SetBody(data)
}

// DeleteExport 删除一个导出产物。
func (h *Template) DeleteExport(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "导出 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.DeleteExport(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// ImportPreview 预览一个模板包。
func (h *Template) ImportPreview(ctx context.Context, c *app.RequestContext) {
	var req struct {
		FileID int64 `json:"file_id"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	view, err := h.svc.ImportPreview(ctx, template.ImportRequest{FileID: req.FileID}, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Import 导入一个模板包。
func (h *Template) Import(ctx context.Context, c *app.RequestContext) {
	var req struct {
		FileID int64 `json:"file_id"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Import(ctx, template.ImportRequest{FileID: req.FileID},
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// Family 返回同一个模板族的全部版本（F-3-04）。
func (h *Template) Family(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.Family(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}
