package handler

import (
	"context"
	"strconv"

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

// Download 取回一份用户存储文件（G-38）。内容经控制面转发，
// 归属校验与审计在服务层；不缓存——减少一份浏览器磁盘缓存里的副本。
func (h *UserStorage) Download(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "文件 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	userID, nodeID := userAndNode(c)

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	name, data, mime, err := h.svc.DownloadFile(ctx, userID, nodeID, id,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	c.Header("Content-Disposition", contentDisposition(name))
	c.Header("Cache-Control", "no-store")
	c.SetContentType(mime)
	c.Response.SetBody(data)
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

// **这里没有「只登记分片」的接口。** 登记只发生在 PutChunkData 内——
// 见 userstorage.Service 里同一处的说明：允许「只登记字节」等于允许调用方
// 声称某几片已收到而磁盘上什么都没有，而那个缺口要到拼接时才暴露。

// PutChunkData 接收一个分片的**字节**（API-148）。
//
// 请求体是**裸字节**而不是 JSON——分片本身就是二进制，把它 base64 包进
// JSON 会让体积涨 33%，而一次 8MiB 的分片会因此多传 2.7MiB，且服务端还要
// 再解一次码。两条路径都不产生额外信息。
//
// 摘要放在查询参数里而不是请求头：它随内容变化，而请求头的语义是"请求的
// 元信息"，一个会变的摘要放在那里容易被中间件按不透明的方式处理。
func (h *UserStorage) PutChunkData(ctx context.Context, c *app.RequestContext) {
	uploadID := c.Param("uploadID")
	// 分片序号是 **0 基**，因此不能走 namedPathID —— 它按「ID 必须大于 0」
	// 校验，会把第一片（序号 0）判成非法：整个上传在第一步就传不上去，
	// 而错误信息「分片序号不合法」听起来像是客户端算错了序号。
	index, err := strconv.Atoi(c.Param("index"))
	if err != nil || index < 0 {
		api.Fail(c, api.InvalidParameter("分片序号不合法"))
		return
	}
	body := c.Request.Body()
	if len(body) == 0 {
		api.Fail(c, api.InvalidParameter("分片内容为空"))
		return
	}
	user := auth.CurrentUser(c)

	view, err := h.svc.UploadChunkData(ctx, user.ID, uploadID, int(index), body, c.Query("sha256"))
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
