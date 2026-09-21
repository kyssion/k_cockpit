package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
)

// DetectIPv6Prefixes 检测某节点外网网卡上的 IPv6 前缀。
func (h *PublicIP) DetectIPv6Prefixes(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	items, err := h.svc.DetectIPv6Prefixes(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// ReloadRules 按当前绑定关系重新应用全部公网地址规则。
func (h *PublicIP) ReloadRules(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	res, err := h.svc.ReloadRules(ctx, nodeID, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, res)
}

// GuestStatus 查询某台虚拟机里实际配置的公网地址。
//
// 它回答"绑定成功之后为什么还是不通"：绑定只保证规则下发，来宾里还要有人
// 把地址配上。
func (h *PublicIP) GuestStatus(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	items, err := h.svc.GuestStatus(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}
