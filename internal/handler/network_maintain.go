package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/network"
)

// --- 交换机迁移与重配置 ---

type migrateSwitchRequest struct {
	UplinkIf    string `json:"uplink_if"`
	VlanID      *int   `json:"vlan_id"`
	Acknowledge bool   `json:"acknowledge"`
}

// MigrateSwitch 迁移交换机到另一块物理网卡。
func (h *Network) MigrateSwitch(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "网络 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req migrateSwitchRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.MigrateSwitch(ctx, id, network.MigrateSwitchRequest{
		UplinkIf: req.UplinkIf, VlanID: req.VlanID, Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// ReconfigureSwitch 按当前配置重新下发。
//
// 它不需要任何参数：参数是"要改成什么"，而重配置做的是"让它与我们记录的
// 一致"。给一个空 body 是对的，不是偷懒。
func (h *Network) ReconfigureSwitch(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "网络 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.ReconfigureSwitch(ctx, id, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// --- 端口释放 ---

type releasePortRequest struct {
	NodeID   int64  `json:"node_id"`
	SwitchID *int64 `json:"switch_id"`
	PortRef  string `json:"port_ref"`
}

// ReleasePort 释放一个 VPC 端口。
func (h *Network) ReleasePort(ctx context.Context, c *app.RequestContext) {
	var req releasePortRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.ReleasePort(ctx, network.ReleasePortRequest{
		NodeID: req.NodeID, SwitchID: req.SwitchID, PortRef: req.PortRef,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// --- 计数重置 ---

// ResetCounters 重置节点上交换机 / 网口的流量计数。
func (h *Network) ResetCounters(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	res, err := h.svc.ResetCounters(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, res)
}

// --- IPv6 ---

type ipv6PolicyRequest struct {
	NodeID          int64    `json:"node_id"`
	Protect         bool     `json:"protect"`
	TrustedPrefixes []string `json:"trusted_prefixes"`
}

// ApplyIPv6Policy 下发 IPv6 保护策略与可信前缀。
func (h *Network) ApplyIPv6Policy(ctx context.Context, c *app.RequestContext) {
	var req ipv6PolicyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.NodeID <= 0 {
		req.NodeID = int64(queryInt(c, "node_id"))
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	res, err := h.svc.ApplyIPv6Policy(ctx, network.IPv6PolicyRequest{
		NodeID: req.NodeID, Protect: req.Protect, TrustedPrefixes: req.TrustedPrefixes,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, res)
}
