package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/quota"
)

// Quota 提供存储配额接口（F-9-02）。
//
// 权限：自己的用量对所有登录用户开放；设置配额与查看全部用户的用量是
// admin 专属（在路由注册时声明）。
type Quota struct {
	svc *quota.Service
}

// NewQuota 构造配额接口。
func NewQuota(svc *quota.Service) *Quota {
	return &Quota{svc: svc}
}

// Mine 返回当前用户在各节点上的用量（API-090）。
//
// 不用传 user_id：**只能查自己的**。让用户传一个 ID 去查别人的用量，就得
// 在服务层再做一次归属校验——而少一个这样的入口，就少一次校验被写错的
// 机会。
func (h *Quota) Mine(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	nodeID := int64(queryInt(c, "node_id"))

	// node_id 为 0 时返回该用户在所有节点上的用量：用户关心的是「我总共
	// 用了多少」，而那通常不限于某一个节点。
	if nodeID > 0 {
		usage, err := h.svc.Usage(ctx, user.ID, nodeID)
		if err != nil {
			api.Fail(c, err)
			return
		}
		api.OK(c, map[string]any{"items": []any{usage}})
		return
	}

	items, err := h.svc.UsageForUser(ctx, user.ID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// List 返回全部配额记录（API-091，管理员）。
func (h *Quota) List(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.List(ctx, int64(queryInt(c, "node_id")))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type setQuotaRequest struct {
	UserID     int64 `json:"user_id"`
	NodeID     int64 `json:"node_id"`
	Enabled    bool  `json:"enabled"`
	QuotaBytes int64 `json:"quota_bytes"`
	ReadOnly   bool  `json:"read_only"`
}

// Set 设置配额（API-092，管理员）。
//
// **同步生效、不需要二次验证**：它是纯控制面元数据，不影响宿主机上的任何
// 东西，而且随时可以改回来。做成任务会制造一个「界面说配额改了、实际还没改」
// 的窗口——那会让管理员以为已经把某个用户限住，而实际上没有。
func (h *Quota) Set(ctx context.Context, c *app.RequestContext) {
	var req setQuotaRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	usage, err := h.svc.Set(ctx, quota.SetQuotaRequest{
		UserID: req.UserID, NodeID: req.NodeID,
		Enabled: req.Enabled, QuotaBytes: req.QuotaBytes, ReadOnly: req.ReadOnly,
	}, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, usage)
}

// 编译期断言：配额接口只对已登录用户开放（admin 判断在路由层）。
var _ = authz.ViewerOf
