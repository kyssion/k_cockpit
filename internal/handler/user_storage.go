package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/userstorage"
)

// UserStorage 提供用户存储空间、文件管理与分片上传（F-5-03/04/05）。
type UserStorage struct {
	svc *userstorage.Service
}

// NewUserStorage 构造接口。
func NewUserStorage(svc *userstorage.Service) *UserStorage {
	return &UserStorage{svc: svc}
}

// userAndNode 取目标用户与节点。
//
// 默认操作自己（这是绝大多数调用）；管理员可以带 `user_id` 参数代管。
func userAndNode(c *app.RequestContext) (userID, nodeID int64) {
	user := auth.CurrentUser(c)
	userID = user.ID
	if v := authz.ViewerOf(c); v.IsAdmin {
		if id := int64(queryInt(c, "user_id")); id > 0 {
			userID = id
		}
	}
	return userID, int64(queryInt(c, "node_id"))
}

// Get 返回存储空间状态（API-140）。
func (h *UserStorage) Get(ctx context.Context, c *app.RequestContext) {
	userID, nodeID := userAndNode(c)

	view, err := h.svc.Get(ctx, userID, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Ensure 开通存储空间（API-141）。
func (h *UserStorage) Ensure(ctx context.Context, c *app.RequestContext) {
	userID, nodeID := userAndNode(c)

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Ensure(ctx, userID, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// ListFiles 返回文件列表（API-142）。
func (h *UserStorage) ListFiles(ctx context.Context, c *app.RequestContext) {
	userID, nodeID := userAndNode(c)

	items, err := h.svc.ListFiles(ctx, userID, nodeID, c.Query("category"))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// DeleteFile 删除文件（API-143）。
func (h *UserStorage) DeleteFile(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "文件 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	userID, nodeID := userAndNode(c)

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.DeleteFile(ctx, userID, nodeID, id,
		authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

type createUploadRequest struct {
	NodeID    int64  `json:"node_id"`
	Category  string `json:"category"`
	RelDir    string `json:"rel_dir"`
	Filename  string `json:"filename"`
	TotalSize int64  `json:"total_size"`
	ChunkSize int    `json:"chunk_size"`
	// SHA256 由客户端计算，用于秒传。留空表示不做秒传。
	SHA256 string `json:"sha256"`
}

// CreateUpload 创建上传会话（API-144）。命中秒传时直接完成。
func (h *UserStorage) CreateUpload(ctx context.Context, c *app.RequestContext) {
	var req createUploadRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreateUpload(ctx, user.ID, userstorage.CreateUploadRequest{
		NodeID: req.NodeID, Category: req.Category, RelDir: req.RelDir,
		Filename: req.Filename, TotalSize: req.TotalSize,
		ChunkSize: req.ChunkSize, SHA256: req.SHA256,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// GetUpload 查询会话状态（API-145），供断点续传。
func (h *UserStorage) GetUpload(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)

	view, err := h.svc.GetUpload(ctx, user.ID, c.Param("uploadID"))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type uploadChunkRequest struct {
	// Index 是分片序号，从 0 开始。
	Index int `json:"index"`
	// SHA256 是该分片的摘要，供节点侧校验。留空表示不校验。
	SHA256 string `json:"sha256"`
}

// UploadChunk 登记一个分片已收到（API-146）。
//
// 重复登记同一分片**不算错误**：网络重试是常态。
func (h *UserStorage) UploadChunk(ctx context.Context, c *app.RequestContext) {
	var req uploadChunkRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)

	view, err := h.svc.UploadChunk(ctx, user.ID, c.Param("uploadID"), req.Index, req.SHA256)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// CompleteUpload 收尾上传（API-147）。
func (h *UserStorage) CompleteUpload(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CompleteUpload(ctx, user.ID, c.Param("uploadID"),
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}
