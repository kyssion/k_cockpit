package handler

import (
	"context"
	"strconv"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/vpcacl"
)

// VpcACL 提供 VPC 网络的访问控制接口（F-4-05）。
type VpcACL struct {
	svc *vpcacl.Service
}

// NewVpcACL 构造 ACL 接口。
func NewVpcACL(svc *vpcacl.Service) *VpcACL { return &VpcACL{svc: svc} }

type aclRuleRequest struct {
	NodeID    int64  `json:"node_id"`
	SwitchID  *int64 `json:"switch_id"`
	Priority  int    `json:"priority"`
	Action    string `json:"action"`
	Direction string `json:"direction"`
	Protocol  string `json:"protocol"`
	SrcCIDR   string `json:"src_cidr"`
	DstCIDR   string `json:"dst_cidr"`
	PortStart *int   `json:"port_start"`
	PortEnd   *int   `json:"port_end"`
	Enabled   *bool  `json:"enabled"`
	Remark    string `json:"remark"`
}

func (r aclRuleRequest) toService() vpcacl.RuleRequest {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	return vpcacl.RuleRequest{
		NodeID: r.NodeID, SwitchID: r.SwitchID,
		Priority: r.Priority, Action: r.Action, Direction: r.Direction, Protocol: r.Protocol,
		SrcCIDR: r.SrcCIDR, DstCIDR: r.DstCIDR, PortStart: r.PortStart, PortEnd: r.PortEnd,
		Enabled: enabled, Remark: r.Remark,
	}
}

// switchIDOf 解析 switch_id 查询参数：为空表示节点级默认规则。
func switchIDOf(c *app.RequestContext) *int64 {
	raw := c.Query("switch_id")
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// List 列出规则。
func (h *VpcACL) List(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	items, err := h.svc.List(ctx, nodeID, switchIDOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items})
}

// Create 新增规则。
func (h *VpcACL) Create(ctx context.Context, c *app.RequestContext) {
	var req aclRuleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Create(ctx, req.toService(), authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Update 修改规则。
func (h *VpcACL) Update(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "规则 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req aclRuleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.Update(ctx, id, req.toService(), authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Delete 删除规则。
func (h *VpcACL) Delete(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "规则 ID")
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
	api.NoContent(c)
}

// Preview 预览规则集。
func (h *VpcACL) Preview(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	view, err := h.svc.Preview(ctx, nodeID, switchIDOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type aclApplyRequest struct {
	NodeID   int64  `json:"node_id"`
	SwitchID *int64 `json:"switch_id"`
	// Version 必须是最近一次预览返回的指纹。
	Version string `json:"version"`
	// Acknowledge 表示已知悉预览中的提示。
	Acknowledge bool `json:"acknowledge"`
}

// Apply 应用规则集。
func (h *VpcACL) Apply(ctx context.Context, c *app.RequestContext) {
	var req aclApplyRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	// node_id 也接受查询参数：预览用 GET，应用用 POST，两处都要求它。
	if req.NodeID <= 0 {
		req.NodeID = int64(queryInt(c, "node_id"))
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Apply(ctx, vpcacl.ApplyRequest{
		NodeID: req.NodeID, SwitchID: req.SwitchID,
		Version: req.Version, Acknowledge: req.Acknowledge,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
