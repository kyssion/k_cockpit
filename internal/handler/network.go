package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/network"
)

// Network 提供网络后端相关接口（F-4-01）。
//
// 权限：网络管理为 **admin 专属**（f-1-06 §3.1），在路由注册时声明。
// M2 只有只读接口——网络变更（建网桥、物理口入桥）属 M3 范围。
type Network struct {
	svc *network.Service
}

// NewNetwork 构造网络接口。
func NewNetwork(svc *network.Service) *Network {
	return &Network{svc: svc}
}

// Status 返回网络后端模式、能力清单与降级说明（API-045）。
//
// 响应刻意**区分三态**（可用 / 不可用 / 未知）而不是布尔值：探测失败与
// 确认缺失是两回事——前者要重试，后者要装东西。混为一谈会让用户去装一个
// 其实已经装好的包（f-4-01 Q-003）。
func (h *Network) Status(ctx context.Context, c *app.RequestContext) {
	nodeID, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	view, err := h.svc.Status(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Networks 返回节点的可用网络列表（API-046）。
func (h *Network) Networks(ctx context.Context, c *app.RequestContext) {
	nodeID, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	views, err := h.svc.Networks(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, views)
}
