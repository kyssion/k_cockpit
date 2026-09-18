package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/hosttuning"
)

// HostTuning 提供宿主机性能调优接口（KSM / ZRAM / 嵌套虚拟化 / CPU 亲和）。
// 归管理员。
//
// 归管理员的理由：这几项改的是**宿主机自己**的行为——ZRAM 会占掉一块物理
// 内存、嵌套虚拟化会向所有来宾暴露虚拟化扩展。租户不该有能力影响同宿主上
// 别人的机器。
type HostTuning struct {
	svc *hosttuning.Service
}

// NewHostTuning 构造接口。
func NewHostTuning(svc *hosttuning.Service) *HostTuning {
	return &HostTuning{svc: svc}
}

// Get 返回调优状态（API-330）。
//
// **开关与效果一起返回**：只给「已启用」的话，用户无法回答两个最实际的问题
// ——该不该开、开了有没有用。KSM 尤其如此：它持续消耗 CPU，在什么都没合并
// 的时候照样扫描内存，因此「开着但一无所获」是一个真实存在、且用户完全看
// 不出来的坏状态。
func (h *HostTuning) Get(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	view, err := h.svc.Get(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type tuningRequest struct {
	Item       string `json:"item"`
	Enabled    *bool  `json:"enabled"`
	DisksizeMB int64  `json:"disksize_mb"`
	Algorithm  string `json:"algorithm"`
	MemLimitMB int64  `json:"mem_limit_mb"`
}

// Apply 修改一项调优（API-331）。
func (h *HostTuning) Apply(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req tuningRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Apply(ctx, hosttuning.Request{
		NodeID: nodeID, Item: req.Item, Enabled: req.Enabled,
		DisksizeMB: req.DisksizeMB, Algorithm: req.Algorithm, MemLimitMB: req.MemLimitMB,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task": t})
}

// ListPresets 返回 CPU 亲和性预设（API-332）。
func (h *HostTuning) ListPresets(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.ListPresets(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// CreatePreset 新建亲和性预设（API-333）。
func (h *HostTuning) CreatePreset(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	var req struct {
		Name   string `json:"name"`
		CPUSet string `json:"cpuset"`
		Remark string `json:"remark"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreatePreset(ctx, hosttuning.PresetRequest{
		NodeID: nodeID, Name: req.Name, CPUSet: req.CPUSet, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DeletePreset 删除预设（API-334）。
func (h *HostTuning) DeletePreset(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	id, err := namedPathID(c, "id", "预设 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.DeletePreset(ctx, nodeID, id, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"ok": true})
}
