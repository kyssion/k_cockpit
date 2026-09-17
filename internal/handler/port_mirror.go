package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/portmirror"
)

// PortMirror 提供端口镜像接口（F-4-09）。
type PortMirror struct {
	svc *portmirror.Service
}

// NewPortMirror 构造接口。
func NewPortMirror(svc *portmirror.Service) *PortMirror {
	return &PortMirror{svc: svc}
}

func mirrorNodeID(c *app.RequestContext) (int64, error) {
	id := int64(queryInt(c, "node_id"))
	if id <= 0 {
		return 0, api.InvalidParameter("必须指定 node_id")
	}
	return id, nil
}

// List 返回节点的镜像规则（API-180）。
func (h *PortMirror) List(ctx context.Context, c *app.RequestContext) {
	nodeID, err := mirrorNodeID(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.List(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

type mirrorRequest struct {
	Name           string   `json:"name"`
	SourcePorts    []string `json:"source_ports"`
	TargetSwitches []string `json:"target_switches"`
	Direction      string   `json:"direction"`
	VlanPreserve   bool     `json:"vlan_preserve"`
}

// Create 新建镜像规则（API-181）。默认不启用。
func (h *PortMirror) Create(ctx context.Context, c *app.RequestContext) {
	nodeID, err := mirrorNodeID(c)
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req mirrorRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Create(ctx, portmirror.Request{
		NodeID: nodeID, Name: req.Name, SourcePorts: req.SourcePorts,
		TargetSwitches: req.TargetSwitches, Direction: req.Direction,
		VlanPreserve: req.VlanPreserve,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Update 修改未启用的规则（API-182）。
func (h *PortMirror) Update(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "镜像 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req mirrorRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Update(ctx, id, portmirror.Request{
		Name: req.Name, SourcePorts: req.SourcePorts,
		TargetSwitches: req.TargetSwitches, Direction: req.Direction,
		VlanPreserve: req.VlanPreserve,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Delete 删除未启用的规则（API-183）。
func (h *PortMirror) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "镜像 ID")
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

// Precheck 返回启用前的静态检查结论（API-184）。
func (h *PortMirror) Precheck(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "镜像 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	warnings, err := h.svc.Precheck(ctx, id)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"warnings": warnings})
}

type enableMirrorRequest struct {
	// WatchdogSeconds 是自动撤销窗口；为 0 时取默认值。
	WatchdogSeconds int `json:"watchdog_seconds"`
	// Acknowledge 表示调用方已看过风险警告并确认继续。
	Acknowledge bool `json:"acknowledge"`
}

// Enable 启用镜像并建立看门狗（API-185）。
//
// **看门狗与镜像在同一次下发里建立**。分两次会留下一个「已生效、没兜底」
// 的窗口，而如果网络恰好在那几秒里被镜像打垮，就再也没有东西能撤销它。
//
// 有风险警告而未确认时不下发、也不报错，而是把警告原样返回——那是一个
// 需要用户做决定的岔路口，不是一次失败。
func (h *PortMirror) Enable(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "镜像 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req enableMirrorRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	result, err := h.svc.Enable(ctx, id, portmirror.EnableRequest{
		WatchdogSeconds: req.WatchdogSeconds, Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, result)
}

// Confirm 确认保持，取消看门狗（API-186）。
//
// **可选动作**：不做它镜像会被自动撤销。安全性靠的是那个默认行为，
// 而不是用户的记性。
func (h *PortMirror) Confirm(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "镜像 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Confirm(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Disable 关闭镜像（API-187）。
//
// **不做确认、不做预检**：一个正在打垮网络的镜像，用户需要的是一键关掉。
func (h *PortMirror) Disable(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "镜像 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Disable(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}
