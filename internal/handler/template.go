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
func (h *Template) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Delete(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
