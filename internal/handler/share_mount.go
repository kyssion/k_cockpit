package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/storage"
)

// ListShares 返回虚拟机的目录共享（API-130）。
func (h *Storage) ListShares(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	items, err := h.svc.ListShares(ctx, vmID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type mountShareRequest struct {
	// RelPath 是**相对于调用者存储根**的路径。
	//
	// 不接受绝对路径，这是整个功能的安全前提：共享是一个跨越虚拟化边界的
	// 读取入口，而参数来自用户；允许绝对路径意味着一个租户可以把 /etc
	// 挂进自己的虚拟机读出来，而这不会触发任何权限检查——qemu 是以一个
	// 有权读它的用户在跑。
	RelPath string `json:"rel_path"`
	Tag     string `json:"tag"`
	// SecurityModel 留空为 mapped（最安全的一档）。
	SecurityModel string `json:"security_model"`
	// ReadOnly 留空为 **true**：可写共享的写入不受控制面配额约束，
	// 因此安全的默认值是不给写。
	ReadOnly *bool `json:"read_only"`
}

// MountShare 把宿主机目录共享给虚拟机（API-131）。
func (h *Storage) MountShare(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req mountShareRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.MountShare(ctx, storage.MountShareRequest{
		VMID:    vmID,
		RelPath: req.RelPath, Tag: req.Tag,
		SecurityModel: req.SecurityModel, ReadOnly: req.ReadOnly,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// UnmountShare 卸载目录共享（API-132）。
//
// **不需要二次验证**：目录还在宿主机上，随时可以再挂回去，而失效是
// 立刻可见的。
func (h *Storage) UnmountShare(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	tag := c.Param("tag")
	if tag == "" {
		api.Fail(c, api.InvalidParameter("必须指定 tag"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UnmountShare(ctx, vmID, tag, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
