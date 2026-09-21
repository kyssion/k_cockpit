package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/useradmin"
)

// UserAdmin 提供用户管理接口（F-1-07）。整体归管理员。
type UserAdmin struct {
	svc *useradmin.Service
}

// NewUserAdmin 构造接口。
func NewUserAdmin(svc *useradmin.Service) *UserAdmin { return &UserAdmin{svc: svc} }

// List 用户列表（API-210）。
func (h *UserAdmin) List(ctx context.Context, c *app.RequestContext) {
	page, err := h.svc.List(ctx, useradmin.ListFilter{
		Keyword: c.Query("keyword"), Status: c.Query("status"), Role: c.Query("role"),
		Page: queryInt(c, "page"), PageSize: queryInt(c, "page_size"),
	})
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, page)
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
	Email    string `json:"email"`
	Remark   string `json:"remark"`
}

// Create 创建用户（API-211）。
func (h *UserAdmin) Create(ctx context.Context, c *app.RequestContext) {
	var req createUserRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Create(ctx, useradmin.CreateRequest{
		Username: req.Username, Password: req.Password, Role: req.Role,
		Email: req.Email, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type updateUserRequest struct {
	Email  *string `json:"email"`
	Remark *string `json:"remark"`
	Role   string  `json:"role"`
	// MaxBandwidthMbps 为 nil 表示不改；0 表示不限（因此不能用 int 的零值
	// 表达"不改"）。
	MaxBandwidthMbps *int `json:"max_bandwidth_mbps"`
}

type sshAccessRequest struct {
	Enabled bool `json:"enabled"`
}

// SetSSHAccess 启用 / 禁用某用户的 SSH 访问（F-1-10）。
func (h *UserAdmin) SetSSHAccess(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "用户 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req sshAccessRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.SetSSHAccess(ctx, id, req.Enabled, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Update 编辑用户（API-212）。**不含密码**——改密是一个独立且更敏感的动作：
// 它会让已登录的会话全部失效，而塞进「编辑资料」里会让一次无心的保存
// 造成一次全员登出。
func (h *UserAdmin) Update(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "用户 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req updateUserRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Update(ctx, id, useradmin.UpdateRequest{
		Email: req.Email, Remark: req.Remark, Role: req.Role,
		MaxBandwidthMbps: req.MaxBandwidthMbps,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type setStatusRequest struct {
	Status string `json:"status"`
}

// SetStatus 封禁或解封（API-213）。
//
// 级联动作里失败的条目在 `warnings` 里返回，而**整体仍是成功**：封禁的实质
// 是置状态，那一步已经生效；后两步是减少暴露面的加固。让加固的失败把整个
// 操作判为失败，会让界面显示「封禁失败」——而那人其实已经被封了。
func (h *UserAdmin) SetStatus(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "用户 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req setStatusRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.SetStatus(ctx, id, req.Status, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// Delete 删除用户（API-214，软删除）。
func (h *UserAdmin) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "用户 ID")
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
