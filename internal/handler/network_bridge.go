package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/networkbridge"
)

// NetworkBridge 提供网络底座接口（F-4-01 / F-4-13）。
type NetworkBridge struct {
	svc *networkbridge.Service
}

// NewNetworkBridge 构造接口。
func NewNetworkBridge(svc *networkbridge.Service) *NetworkBridge {
	return &NetworkBridge{svc: svc}
}

// Overview 返回网络底座的完整状态（API-190）。
//
// **它几乎不会失败**：探测不到节点、桥列表读不出来，都以字段形式返回，
// 整体仍是 200。这是规格里「网络配置失败不得阻断主流程」的具体实现——
// 用户点进这个页面，本来就是为了看网络出了什么问题；如果这里给他一个
// 白屏或 500，他连「网络坏了」这个结论都拿不到。
func (h *NetworkBridge) Overview(ctx context.Context, c *app.RequestContext) {
	nodeID, err := mirrorNodeID(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	view, err := h.svc.Overview(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// List 只返回桥列表（API-191）。
func (h *NetworkBridge) List(ctx context.Context, c *app.RequestContext) {
	nodeID, err := mirrorNodeID(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.ListBridges(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type bridgeRequest struct {
	Name        string `json:"name"`
	Backend     string `json:"backend"`
	Mode        string `json:"mode"`
	CIDR        string `json:"cidr"`
	GatewayIP   string `json:"gateway_ip"`
	DHCPStart   string `json:"dhcp_start"`
	DHCPEnd     string `json:"dhcp_end"`
	DHCPEnabled bool   `json:"dhcp_enabled"`
	VlanID      *int   `json:"vlan_id"`
	Remark      string `json:"remark"`
}

// CreateBridge 新建网络（API-192）。**不接物理口**——那是一个独立且更危险的操作。
func (h *NetworkBridge) CreateBridge(ctx context.Context, c *app.RequestContext) {
	nodeID, err := mirrorNodeID(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req bridgeRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.CreateBridge(ctx, networkbridge.BridgeRequest{
		NodeID: nodeID, Name: req.Name, Backend: req.Backend, Mode: req.Mode,
		CIDR: req.CIDR, GatewayIP: req.GatewayIP,
		DHCPStart: req.DHCPStart, DHCPEnd: req.DHCPEnd,
		DHCPEnabled: req.DHCPEnabled, VlanID: req.VlanID, Remark: req.Remark,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DeleteBridge 删除网络（API-193）。系统预置网络不可删。
func (h *NetworkBridge) DeleteBridge(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "网络 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	if err := h.svc.DeleteBridge(ctx, id, authz.ViewerOf(c), user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": true})
}

type attachUplinkRequest struct {
	UplinkIf string `json:"uplink_if"`
	// WatchdogSeconds 是自动回滚窗口；为 0 时取默认值。**不能设为「无」**。
	WatchdogSeconds int `json:"watchdog_seconds"`
	// Acknowledge 表示调用方已看过风险警告并确认继续。
	Acknowledge bool `json:"acknowledge"`
}

// AttachUplink 物理口入桥（API-194）。
//
// **两道防线**：显式确认防「没想清楚就点了」，自动回滚窗口防「确认过之后
// 才发现不行」——口加进去了、网络断了、面板打不开了。第二道的执行者在
// **节点侧**，因为控制面是通过网络下发指令的，而这个操作的结果恰恰可能
// 是网络断掉。
func (h *NetworkBridge) AttachUplink(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "网络 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req attachUplinkRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.AttachUplink(ctx, id, req.UplinkIf, req.WatchdogSeconds,
		req.Acknowledge, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// ConfirmUplink 确认保持（API-195）。
func (h *NetworkBridge) ConfirmUplink(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "网络 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.ConfirmUplink(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// DetachUplink 摘出物理口（API-196）。**不做确认、不做预检**。
func (h *NetworkBridge) DetachUplink(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "网络 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.DetachUplink(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Repair 修复（API-197）。
//
// 它是一个**幂等的收敛动作**，而不是「重试上一次失败的操作」：重试一件已经
// 失败的事通常不会得到不同结果，而收敛会先看清现状再补差异。
//
// 修复本身失败时**不返回错误**，而是写进 Remaining——用户点「修复」多半是
// 因为网络已经坏了，此时再收到一个 500 对他没有任何帮助。
func (h *NetworkBridge) Repair(ctx context.Context, c *app.RequestContext) {
	nodeID, err := mirrorNodeID(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.Repair(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}
