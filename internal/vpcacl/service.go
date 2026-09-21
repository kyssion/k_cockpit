// Package vpcacl 提供 VPC 网络的访问控制规则（F-4-05）。
//
// 作用域是**交换机（网段）**，与安全组的区别在于：安全组挂在虚拟机网口上，
// ACL 挂在一个网段上。"这个网段整体上不许访问某个地址"用 ACL 表达一次即可，
// 逐台配安全组既重复又容易漏。
package vpcacl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Service 提供 ACL 能力。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	queue *task.Queue
	audit *audit.Recorder
}

// NewService 构造 ACL 服务。
func NewService(
	db *gorm.DB, client agent.Client, queue *task.Queue, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, agent: client, queue: queue, audit: recorder}
}

// --- 规则视图 ---

// RuleView 是一条规则的对外形态。
type RuleView struct {
	ID        int64  `json:"id"`
	NodeID    int64  `json:"node_id"`
	SwitchID  *int64 `json:"switch_id,omitempty"`
	Priority  int    `json:"priority"`
	Action    string `json:"action"`
	Direction string `json:"direction"`
	Protocol  string `json:"protocol"`
	SrcCIDR   string `json:"src_cidr,omitempty"`
	DstCIDR   string `json:"dst_cidr,omitempty"`
	PortStart *int   `json:"port_start,omitempty"`
	PortEnd   *int   `json:"port_end,omitempty"`
	Enabled   bool   `json:"enabled"`
	Remark    string `json:"remark,omitempty"`
	// MatchesAll 提示这条匹配全部地址。
	//
	// 界面用它给"匹配全部 + deny"的规则加提示：那样一条若排在前面，后面的
	// 规则永远不会生效——那是"设了白名单却全不通"最常见的原因。
	MatchesAll bool `json:"matches_all"`
}

// RuleRequest 是一次规则的写入请求。
type RuleRequest struct {
	NodeID    int64
	SwitchID  *int64
	Priority  int
	Action    string
	Direction string
	Protocol  string
	SrcCIDR   string
	DstCIDR   string
	PortStart *int
	PortEnd   *int
	Enabled   bool
	Remark    string
}

// List 列出某交换机（或某节点的默认）规则。
func (s *Service) List(ctx context.Context, nodeID int64, switchID *int64) ([]RuleView, error) {
	q := s.db.WithContext(ctx).Model(&model.VpcACLRule{}).Where("node_id = ?", nodeID)
	if switchID != nil {
		q = q.Where("switch_id = ?", *switchID)
	} else {
		q = q.Where("switch_id IS NULL")
	}
	var rows []model.VpcACLRule
	if err := q.Order("priority ASC, id ASC").Find(&rows).Error; err != nil {
		log.Printf("[vpcacl] 查询规则失败: %v", err)
		return nil, api.Internal()
	}
	out := make([]RuleView, 0, len(rows))
	for i := range rows {
		out = append(out, toRuleView(&rows[i]))
	}
	return out, nil
}

// Create 新增一条规则。
func (s *Service) Create(
	ctx context.Context, req RuleRequest, v authz.Viewer, operatorName, clientIP string,
) (*RuleView, error) {
	row, err := s.fromRequest(req)
	if err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		log.Printf("[vpcacl] 创建规则失败: %v", err)
		return nil, api.Internal()
	}
	s.record(ctx, v.UserID, operatorName, clientIP, "vpc_acl.rule.create", row.NodeID, row.ID, true, "")
	view := toRuleView(row)
	return &view, nil
}

// Update 修改一条规则。
func (s *Service) Update(
	ctx context.Context, id int64, req RuleRequest, v authz.Viewer, operatorName, clientIP string,
) (*RuleView, error) {
	var row model.VpcACLRule
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("规则不存在")
	case err != nil:
		log.Printf("[vpcacl] 查询规则失败: %v", err)
		return nil, api.Internal()
	}

	next, err := s.fromRequest(req)
	if err != nil {
		return nil, err
	}
	next.ID = row.ID
	next.CreatedAt = row.CreatedAt
	if err := s.db.WithContext(ctx).Model(&model.VpcACLRule{}).
		Where("id = ?", id).Updates(updatesOf(next)).Error; err != nil {
		log.Printf("[vpcacl] 更新规则失败: %v", err)
		return nil, api.Internal()
	}
	final := *next
	s.record(ctx, v.UserID, operatorName, clientIP, "vpc_acl.rule.update", final.NodeID, id, true, "")
	view := toRuleView(&final)
	return &view, nil
}

// Delete 删除一条规则。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	res := s.db.WithContext(ctx).Where("id = ?", id).Delete(&model.VpcACLRule{})
	if res.Error != nil {
		log.Printf("[vpcacl] 删除规则失败: %v", res.Error)
		return api.Internal()
	}
	if res.RowsAffected == 0 {
		return api.NotFound("规则不存在")
	}
	s.record(ctx, v.UserID, operatorName, clientIP, "vpc_acl.rule.delete", 0, id, true, "")
	return nil
}

// --- 预览与应用 ---

// PreviewView 是一次预览的结果。
type PreviewView struct {
	NodeID   int64  `json:"node_id"`
	SwitchID *int64 `json:"switch_id,omitempty"`
	// Rules 是本次会生效的规则（按顺序）。
	Rules []RuleView `json:"rules"`
	// Rendered 是节点上真正会生成的条目。
	//
	// 规则集从"我填了什么"到"实际生效什么"中间隔着一层归并；不展示这一步，
	// 用户只能在网络不通之后才发现问题。
	Rendered []string `json:"rendered"`
	// Warnings 是隐患提示。
	Warnings []string `json:"warnings"`
	// Version 是这次预览的指纹；**应用必须带回它**。
	//
	// 不带版本号的话，预览之后规则又被改动时按旧预览去应用，等于用一个没人
	// 看过的结论去改真实网络。安全组（F-4-04）用的是同一条约定。
	Version string `json:"version"`
	// Unavailable 非空表示节点不支持 ACL。
	Unavailable string `json:"unavailable,omitempty"`
}

// Preview 预览某交换机（或节点默认）的规则集。
func (s *Service) Preview(ctx context.Context, nodeID int64, switchID *int64) (*PreviewView, error) {
	rules, err := s.List(ctx, nodeID, switchID)
	if err != nil {
		return nil, err
	}
	view := &PreviewView{
		NodeID: nodeID, SwitchID: switchID, Rules: rules,
		Version: versionOf(rules),
	}
	// 隐患在控制面就能判定的先判掉：它们与节点是否支持无关，而用户要在
	// 应用**之前**看见。
	view.Warnings = shadowWarnings(rules)

	if s.agent == nil {
		view.Unavailable = "未连接节点"
		return view, nil
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVpcACLPreview,
		NodeID: nodeID,
		Target: strconv.FormatInt(nodeID, 10),
		Params: map[string]any{"spec": specsOf(rules)},
	})
	if err != nil || result == nil || !result.Success || result.Data == nil {
		view.Unavailable = "节点不支持 ACL 预览"
		return view, nil
	}
	preview := decodePreview(result.Data)
	view.Rendered = preview.Rendered
	view.Warnings = append(view.Warnings, preview.Warnings...)
	if preview.Unavailable != "" {
		view.Unavailable = preview.Unavailable
	}
	if preview.Version != "" {
		// 以节点的指纹为准：它才是"应用时会按哪个集合生效"的权威。
		view.Version = preview.Version
	}
	return view, nil
}

// ApplyRequest 是一次应用请求。
type ApplyRequest struct {
	NodeID   int64
	SwitchID *int64
	// Version 必须是最近一次预览返回的指纹。
	Version string
	// Acknowledge 表示用户已知悉风险（例如规则集里存在"拒绝一切"）。
	Acknowledge bool
}

// Apply 应用规则集。
func (s *Service) Apply(
	ctx context.Context, req ApplyRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	// 应用前**再预览一次**并比对版本号：预览与应用之间可能隔了几分钟，
	// 而这期间规则完全可能被改动。
	preview, err := s.Preview(ctx, req.NodeID, req.SwitchID)
	if err != nil {
		return nil, err
	}
	if preview.Unavailable != "" {
		return nil, api.Unavailable(preview.Unavailable)
	}
	if preview.Version != "" && req.Version != "" && preview.Version != req.Version {
		return nil, api.Conflict("规则已在你预览之后发生变化，请重新预览后再应用")
	}
	if !req.Acknowledge && len(preview.Warnings) > 0 {
		return nil, api.Conflict("存在需要注意的规则：" + strings.Join(preview.Warnings, "；"))
	}

	rules := preview.Rules
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVpcACLApply,
		NodeID:       req.NodeID,
		ResourceType: "vpc_acl",
		ResourceName: "VPC ACL",
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: aclParams{
			NodeID: req.NodeID, SwitchID: req.SwitchID,
			Version: req.Version, Spec: specsOf(rules),
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "vpc_acl.apply", req.NodeID, 0, true, "task:"+strconv.FormatInt(t.ID, 10))
	return t, nil
}

// --- 内部 ---

func (s *Service) fromRequest(req RuleRequest) (*model.VpcACLRule, error) {
	if req.NodeID <= 0 {
		return nil, api.InvalidParameter("必须指定节点")
	}
	switch req.Action {
	case model.AclActionAllow, model.AclActionDeny:
	default:
		return nil, api.InvalidParameter("动作必须是 allow 或 deny")
	}
	switch req.Direction {
	case model.AclDirectionIn, model.AclDirectionOut:
	default:
		return nil, api.InvalidParameter("方向必须是 in 或 out")
	}
	proto := strings.ToLower(strings.TrimSpace(req.Protocol))
	if proto == "" {
		proto = model.AclProtocolAny
	}
	switch proto {
	case model.AclProtocolAny, "tcp", "udp", "icmp":
	default:
		return nil, api.InvalidParameter("协议必须是 any / tcp / udp / icmp")
	}
	src := strings.TrimSpace(req.SrcCIDR)
	if src != "" {
		if _, _, err := net.ParseCIDR(src); err != nil {
			return nil, api.InvalidParameter("源地址不是合法的 CIDR：" + src)
		}
	}
	dst := strings.TrimSpace(req.DstCIDR)
	if dst != "" {
		if _, _, err := net.ParseCIDR(dst); err != nil {
			return nil, api.InvalidParameter("目标地址不是合法的 CIDR：" + dst)
		}
	}
	if req.PortStart != nil && req.PortEnd != nil && *req.PortEnd < *req.PortStart {
		return nil, api.InvalidParameter("结束端口不能小于起始端口")
	}
	if req.Priority < 0 || req.Priority > 100000 {
		return nil, api.InvalidParameter("优先级需在 0 到 100000 之间")
	}

	row := &model.VpcACLRule{
		NodeID: req.NodeID, SwitchID: req.SwitchID,
		Priority: req.Priority, Action: req.Action, Direction: req.Direction, Protocol: proto,
		PortStart: req.PortStart, PortEnd: req.PortEnd, Enabled: req.Enabled,
	}
	if src != "" {
		row.SrcCIDR = &src
	}
	if dst != "" {
		row.DstCIDR = &dst
	}
	if r := strings.TrimSpace(req.Remark); r != "" {
		row.Remark = &r
	}
	return row, nil
}

func updatesOf(r *model.VpcACLRule) map[string]any {
	return map[string]any{
		"node_id": r.NodeID, "switch_id": r.SwitchID,
		"priority": r.Priority, "action": r.Action, "direction": r.Direction, "protocol": r.Protocol,
		"src_cidr": r.SrcCIDR, "dst_cidr": r.DstCIDR,
		"port_start": r.PortStart, "port_end": r.PortEnd,
		"enabled": r.Enabled, "remark": r.Remark, "updated_at": time.Now(),
	}
}

// shadowWarnings 给出**控制面就能判定**的隐患。
//
// 只判一类：前面已经有一条"匹配全部且拒绝"的规则，后面的规则永远不会生效。
// 这是"设了白名单却全不通"最常见的原因，而它的表现与"网络坏了"没有区别。
func shadowWarnings(rules []RuleView) []string {
	out := []string{}
	for i := range rules {
		r := &rules[i]
		if !r.Enabled || r.Action != model.AclActionDeny || !r.MatchesAll {
			continue
		}
		if i+1 < len(rules) {
			out = append(out, "第 "+strconv.Itoa(i+1)+" 条规则会拒绝全部流量，它之后的规则都不会生效")
		}
		break // 只报第一条：它之后的都已被这一条遮住
	}
	return out
}

func toRuleView(r *model.VpcACLRule) RuleView {
	view := RuleView{
		ID: r.ID, NodeID: r.NodeID, SwitchID: r.SwitchID,
		Priority: r.Priority, Action: r.Action, Direction: r.Direction, Protocol: r.Protocol,
		PortStart: r.PortStart, PortEnd: r.PortEnd, Enabled: r.Enabled,
		MatchesAll: r.MatchesAll(),
	}
	if r.SrcCIDR != nil {
		view.SrcCIDR = *r.SrcCIDR
	}
	if r.DstCIDR != nil {
		view.DstCIDR = *r.DstCIDR
	}
	if r.Remark != nil {
		view.Remark = *r.Remark
	}
	return view
}

func specsOf(rules []RuleView) []agent.VpcACLRuleSpec {
	out := make([]agent.VpcACLRuleSpec, 0, len(rules))
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		out = append(out, agent.VpcACLRuleSpec{
			Priority: r.Priority, Action: r.Action, Direction: r.Direction, Protocol: r.Protocol,
			SrcCIDR: r.SrcCIDR, DstCIDR: r.DstCIDR,
			PortStart: derefInt(r.PortStart), PortEnd: derefInt(r.PortEnd),
		})
	}
	return out
}

// versionOf 计算规则集的指纹。
//
// 只覆盖"会影响生效结果"的字段：备注与创建时间变了不该让版本号变，否则
// 改一句备注就得重新预览一次。
func versionOf(rules []RuleView) string {
	h := sha256.New()
	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		_, _ = h.Write([]byte(strconv.Itoa(r.Priority) + "|" + r.Action + "|" + r.Direction + "|" +
			r.Protocol + "|" + r.SrcCIDR + "|" + r.DstCIDR + "|" +
			strconv.Itoa(derefInt(r.PortStart)) + "-" + strconv.Itoa(derefInt(r.PortEnd)) + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func decodePreview(data map[string]any) agent.VpcACLPreview {
	raw, ok := data[agent.VpcACLPreviewDataKey]
	if !ok {
		return agent.VpcACLPreview{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.VpcACLPreview{}
	}
	var p agent.VpcACLPreview
	_ = json.Unmarshal(blob, &p)
	return p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Service) record(
	ctx context.Context, operatorID int64, operatorName, clientIP, action string,
	nodeID, resourceID int64, success bool, detail string,
) {
	if s.audit == nil {
		return
	}
	params := map[string]any{}
	if nodeID > 0 {
		params["node_id"] = nodeID
	}
	if detail != "" {
		params["detail"] = detail
	}
	s.audit.Record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "vpc_acl", ResourceID: resourceID,
		Action: action, Params: params, Success: success, ClientIP: clientIP,
	})
}
