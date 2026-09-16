package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/network"
)

// Network 提供网络后端相关接口（F-4-01 / F-4-02）。
//
// 权限：网络管理为 **admin 专属**（f-1-06 §3.1），在路由注册时声明。
// 读接口只探测节点，写接口（交换机增删改）走任务队列。
type Network struct {
	svc *network.Service
}

// NewNetwork 构造网络接口。
func NewNetwork(svc *network.Service) *Network {
	return &Network{svc: svc}
}

// switchRequest 是交换机新建 / 修改的请求体。
type switchRequest struct {
	Name string `json:"name"`
	// Mode 留空时按 nat 处理——这是最常见的选择，且是系统基础网络的模式。
	Mode      string `json:"mode"`
	VlanID    *int   `json:"vlan_id"`
	CIDR      string `json:"cidr"`
	GatewayIP string `json:"gateway_ip"`
	DHCPStart string `json:"dhcp_start"`
	DHCPEnd   string `json:"dhcp_end"`
	UplinkIf  string `json:"uplink_if"`
}

func (r *switchRequest) toService() network.SwitchRequest {
	return network.SwitchRequest{
		Name: r.Name, Mode: r.Mode, VlanID: r.VlanID, CIDR: r.CIDR,
		GatewayIP: r.GatewayIP, DHCPStart: r.DHCPStart, DHCPEnd: r.DHCPEnd,
		UplinkIf: r.UplinkIf,
	}
}

// CreateSwitch 在节点上新建一个虚拟交换机。
//
// 返回任务标识而非创建好的交换机：建网桥是宿主机上的实际操作，接口不同步
// 等待（f-7-01 R-001）。记录由执行器在节点成功后写入，因此**受理成功的这一刻
// 它还不存在于列表里**——界面据此显示「正在创建」而不是直接去列表里找不到。
func (h *Network) CreateSwitch(ctx context.Context, c *app.RequestContext) {
	nodeID, err := namedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req switchRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.CreateSwitch(ctx, nodeID, req.toService(), user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// UpdateSwitch 修改交换机。
func (h *Network) UpdateSwitch(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "交换机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req switchRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.UpdateSwitch(ctx, id, req.toService(), user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}

// DeleteSwitch 删除交换机。
//
// 占用检查在服务层**同步**完成：把一个还有虚拟机接入的交换机排进队列，
// 等执行到它时才失败，用户会在几分钟后收到一条失败通知，而那时他多半已经
// 去做别的事了。
func (h *Network) DeleteSwitch(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "交换机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.DeleteSwitch(ctx, id, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
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
