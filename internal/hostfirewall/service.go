// Package hostfirewall 实现宿主机防火墙（F-4-11 第一层）。
//
// 它与 `internal/firewall`（KVM 网络防火墙）是**两套东西**，而这一点是本包
// 最需要说清楚的事：
//
//	本包        保护**宿主机自己与面板**——SSH、面板端口、节点上直接对外
//	            的服务。它防的是"谁能登进这台机器"。
//	firewall    保护**虚拟机**——作用在所有虚拟机的入站流量上。它防的是
//	            "谁能访问虚拟机里的服务"。
//
// 两者混为一谈的后果不是代码难看，是**用户会以为改了 KVM 规则就关掉了面板
// 的暴露面**。它们的配置项长得几乎一样，而攻击面完全不同。
//
// 本包比 KVM 那层更危险一点：它挡住的正是 SSH 与面板本身，没有任何"从里面
// 绕过去"的余地——管理员一旦被自己的规则挡在外面，就只剩进机房这一条路。
// 因此两处设计都围绕这一点：**管理白名单优先于一切拒绝**，以及**保护规则
// 不可删改**。
package hostfirewall

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
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

// Service 提供宿主机防火墙能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
	now   func() time.Time

	// panelPort 是面板自身的监听端口，用于合成保护规则。
	//
	// **算出来而不是存进表**：端口是配置项，改配置之后表里那条就过期了，
	// 而一条过期的"保护规则"比没有更糟——它保护着一个不再监听的端口，
	// 而真正的端口没有任何规则挡着。
	panelPort int
	// sshPortsFromSettings 由装配处注入（默认 22）。
	sshPorts []int
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
	panelPort int, sshPorts []int,
) *Service {
	if panelPort <= 0 {
		panelPort = 8080
	}
	if len(sshPorts) == 0 {
		sshPorts = []int{22}
	}
	return &Service{
		db: db, queue: queue, agent: client, audit: recorder, now: time.Now,
		panelPort: panelPort, sshPorts: sshPorts,
	}
}

// View 是规则的对外视图。
type View struct {
	ID         int64  `json:"id"`
	NodeID     int64  `json:"node_id"`
	Action     string `json:"action"`
	Protocol   string `json:"protocol"`
	PortStart  *int   `json:"port_start,omitempty"`
	PortEnd    *int   `json:"port_end,omitempty"`
	SourceCIDR string `json:"source_cidr,omitempty"`

	GeoipRegions []string `json:"geoip_regions,omitempty"`
	IsProtected  bool     `json:"is_protected"`
	Applied      bool     `json:"applied"`
	Remark       string   `json:"remark,omitempty"`
	// Fixed 为 true 表示这条规则**不在表里**，是由面板按当前配置合成的
	// （面板端口、SSH）。界面要据此说明"它跟着配置走，不能改"。
	Fixed       bool   `json:"fixed"`
	FixedReason string `json:"fixed_reason,omitempty"`
	Describe    string `json:"describe"`
}

// PolicyView 是策略的对外视图。
type PolicyView struct {
	NodeID         int64    `json:"node_id"`
	Enabled        bool     `json:"enabled"`
	DefaultAction  string   `json:"default_action"`
	Whitelist      []string `json:"whitelist"`
	Version        int      `json:"version"`
	AppliedAt      string   `json:"applied_at,omitempty"`
	LastRollbackAt string   `json:"last_rollback_at,omitempty"`
	// NeedApply 为 true 表示有改动未生效。
	NeedApply bool `json:"need_apply"`
}

// GetPolicy 返回策略与规则。
func (s *Service) GetPolicy(ctx context.Context, nodeID int64) (*PolicyView, []View, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}

	var rows []model.HostFirewallRule
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("is_protected DESC, action DESC, id ASC").Find(&rows).Error; err != nil {
		log.Printf("[hostfirewall] 查询规则失败: %v", err)
		return nil, nil, api.Internal()
	}

	views := make([]View, 0, len(rows)+2)
	// 合成的保护规则排在最前：它们是"基线里的基线"，而列表开头是视线
	// 最先落到的地方。
	views = append(views, s.fixedRules(nodeID)...)
	for i := range rows {
		views = append(views, toView(&rows[i]))
	}

	pv := &PolicyView{
		NodeID: policy.NodeID, Enabled: policy.Enabled,
		DefaultAction: policy.DefaultAction,
		Whitelist:     splitList(policy.Whitelist),
		Version:       policy.Version,
		// 有规则没下发也算"需要应用"——只比 version 会漏掉这种情况。
		NeedApply: hasUnapplied(rows),
	}
	if policy.AppliedAt != nil {
		pv.AppliedAt = policy.AppliedAt.Format(time.RFC3339)
	}
	if policy.LastRollbackAt != nil {
		pv.LastRollbackAt = policy.LastRollbackAt.Format(time.RFC3339)
	}
	return pv, views, nil
}

// UpdatePolicy 更新策略。
func (s *Service) UpdatePolicy(
	ctx context.Context, nodeID int64, req PolicyRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*PolicyView, error) {
	if req.DefaultAction != "" &&
		req.DefaultAction != model.FirewallAccept && req.DefaultAction != model.FirewallDeny {
		return nil, api.InvalidParameter("默认处置只能是 accept 或 deny")
	}
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{"version": gorm.Expr("version + 1")}
	if req.DefaultAction != "" {
		updates["default_action"] = req.DefaultAction
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.Whitelist != nil {
		joined := strings.Join(normalizeCIDRs(*req.Whitelist), "\n")
		updates["whitelist"] = joined
	}
	if err := s.db.WithContext(ctx).Model(&model.HostFirewallPolicy{}).
		Where("id = ?", policy.ID).Updates(updates).Error; err != nil {
		log.Printf("[hostfirewall] 更新策略失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "host_firewall",
		ResourceID: policy.ID, Action: "host_firewall.policy.update",
		Params: map[string]any{
			"default_action": req.DefaultAction,
			"enabled":        req.Enabled,
			// 白名单入审计：它是**唯一一条能从错误规则里把人救回来的路**，
			// 而"谁在什么时候把它设成了什么"是事故后最先要问的。
			"whitelist": derefStr(req.Whitelist),
		},
		Success: true, ClientIP: clientIP,
	})

	pv, _, err := s.GetPolicy(ctx, nodeID)
	return pv, err
}

// PolicyRequest 是更新策略的请求。
type PolicyRequest struct {
	Enabled       *bool
	DefaultAction string
	Whitelist     *string
}

// RuleRequest 是新建规则的请求。
type RuleRequest struct {
	NodeID       int64
	Action       string
	Protocol     string
	PortStart    *int
	PortEnd      *int
	SourceCIDR   *string
	GeoipRegions []string
	Remark       string
}

// CreateRule 新建一条规则。
//
// 新建的规则**永远不是保护的**：保护标记只能由服务端按配置合成，不接受
// 客户端传入——否则用户可以把一条保护规则改成不保护，删掉它，然后把自己
// 锁在门外，而整个过程看起来完全合法。
func (s *Service) CreateRule(
	ctx context.Context, req RuleRequest, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	if req.Action != model.FirewallAccept && req.Action != model.FirewallDeny {
		return nil, api.InvalidParameter("动作只能是 accept 或 deny")
	}
	switch req.Protocol {
	case "", "tcp", "udp", "icmp":
	default:
		return nil, api.InvalidParameter("协议只能是 tcp / udp / icmp")
	}
	if err := validatePorts(req.Protocol, req.PortStart, req.PortEnd); err != nil {
		return nil, err
	}

	row := model.HostFirewallRule{
		NodeID: req.NodeID, Action: req.Action, Protocol: orDefault(req.Protocol, "tcp"),
		PortStart: req.PortStart, PortEnd: req.PortEnd,
		SourceCIDR: req.SourceCIDR,
		// 显式置 false，见方法注释。
		IsProtected: false,
	}
	if len(req.GeoipRegions) > 0 {
		g := strings.Join(req.GeoipRegions, ",")
		row.GeoipRegions = &g
	}
	if req.Remark != "" {
		row.Remark = &req.Remark
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[hostfirewall] 创建规则失败: %v", err)
		return nil, api.Internal()
	}
	s.bumpVersion(ctx, req.NodeID)

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "host_firewall",
		ResourceID: row.ID, ResourceName: row.Describe(),
		Action: "host_firewall.rule.create",
		Params: map[string]any{
			"action": req.Action, "protocol": row.Protocol,
			"port_start": row.PortStart, "port_end": row.PortEnd,
			"source_cidr": derefStr(row.SourceCIDR),
		},
		Success: true, ClientIP: clientIP,
	})

	view := toView(&row)
	return &view, nil
}

// DeleteRule 删除一条规则。
func (s *Service) DeleteRule(
	ctx context.Context, nodeID, ruleID int64, v authz.Viewer, operatorName, clientIP string,
) error {
	var row model.HostFirewallRule
	if err := s.db.WithContext(ctx).
		Where("id = ? AND node_id = ?", ruleID, nodeID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("规则不存在")
		}
		return api.Internal()
	}

	// **服务端强制保护**，不看界面传了什么。
	//
	// 让界面决定能不能删，等于把一个**不可恢复**的操作交给一次点击：
	// 删掉保护规则之后，管理员可能立刻连不上面板，而那时他已经没有界面
	// 可以用来把它加回来。
	if row.IsProtected {
		return api.Conflict("这是保护规则，不能删除——它保护的是你正在使用的管理通道")
	}
	if err := s.db.WithContext(ctx).Delete(&model.HostFirewallRule{}, row.ID).Error; err != nil {
		return api.Internal()
	}
	s.bumpVersion(ctx, nodeID)

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "host_firewall",
		ResourceID: row.ID, ResourceName: row.Describe(),
		Action:  "host_firewall.rule.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Precheck 预览将要下发的规则。**只读**。
func (s *Service) Precheck(ctx context.Context, nodeID int64) ([]string, []string, error) {
	policy, rules, err := s.GetPolicy(ctx, nodeID)
	if err != nil {
		return nil, nil, err
	}

	preview := []string{fmt.Sprintf(
		"# 宿主机防火墙：默认 %s，白名单 %d 条",
		policy.DefaultAction, len(policy.Whitelist))}
	for _, w := range policy.Whitelist {
		preview = append(preview, fmt.Sprintf(
			"iptables -A INPUT -s %s -j ACCEPT   # 管理白名单，优先于一切拒绝", w))
	}
	preview = append(preview, fmt.Sprintf(
		"iptables -P INPUT %s   # 不匹配任何规则时", strings.ToUpper(policy.DefaultAction)))

	warnings := []string{}
	for _, r := range rules {
		line := "iptables -A INPUT"
		if r.Protocol != "" {
			line += " -p " + r.Protocol
		}
		if r.PortStart != nil {
			if r.PortEnd != nil && *r.PortEnd != *r.PortStart {
				line += fmt.Sprintf(" --dport %d:%d", *r.PortStart, *r.PortEnd)
			} else {
				line += fmt.Sprintf(" --dport %d", *r.PortStart)
			}
		}
		if r.SourceCIDR != "" {
			line += " -s " + r.SourceCIDR
		}
		line += " -j " + strings.ToUpper(r.Action)
		if r.IsProtected {
			line += "   # 保护规则"
		}
		preview = append(preview, line)
	}

	// 白名单为空而默认拒绝：这是最危险的一种组合，必须显式警告。
	if policy.DefaultAction == model.FirewallDeny && len(policy.Whitelist) == 0 {
		warnings = append(warnings,
			"白名单为空且默认拒绝：**任何来源都无法再连上这台机器**，包括 SSH 与面板。"+
				"一旦应用，你只能到宿主机本地（或机房）把规则改回来。"+
				"如果是从固定地址管理这台机器，请先把它加进白名单。")
	}
	return preview, warnings, nil
}

// Apply 应用当前配置到宿主机。
//
// 覆盖式下发（用一份完整的期望状态替换现有规则），而这些规则在节点上是
// **追加**到已有的 INPUT 链上——因此必须**先清掉本系统上次写入的那一段**，
// 否则每应用一次就多一份重复规则，链条越来越长而没人发现。
func (s *Service) Apply(
	ctx context.Context, nodeID, version int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	pv, rules, err := s.GetPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	// **版本必须匹配**（与 f-4-04 同一模式）：用户看到预览、判断没问题、
	// 点确认，而中间别人完全可能改了策略。不校验的话，他批准的是 A、
	// 落下去的是 B——而这类问题不报错。
	//
	// **没有"不传版本就跳过校验"这一说。** 早期版本写了 `version != 0 && ...`，
	// 而它是个绕过：调用方只要省略 version 就完全得不到保护，而接口看上去
	// 一切正常。这类"为了便利留的后门"正是校验最容易被架空的地方。
	if int(version) != pv.Version {
		return nil, api.Conflict("配置已变化，请重新预览后再应用")
	}

	spec := buildSpec(rules)
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskHostFirewallApply,
		NodeID:       nodeID,
		ResourceType: "host_firewall",
		ResourceID:   nodeID,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"action":         "apply",
			"enabled":        pv.Enabled,
			"default_action": pv.DefaultAction,
			"whitelist":      pv.Whitelist,
			"rules":          spec,
		},
	})
	if err != nil {
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "host_firewall",
		Action: "host_firewall.apply",
		Params: map[string]any{
			"version": pv.Version, "rule_count": len(rules),
			"whitelist_count": len(pv.Whitelist),
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// Rollback 紧急回滚：撤掉本系统写入的全部规则，回到"没有宿主机防火墙"的状态。
//
// 它是**最后一道保险**：应用之后连不上面板 / SSH 断了，而用户又进不去宿主机
// 时，这是唯一的自救入口。因此它**不校验版本、不要求二次验证**——它要在
// "已经出事了"的那一刻还能用。
func (s *Service) Rollback(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskHostFirewallApply,
		NodeID:       nodeID,
		ResourceType: "host_firewall",
		ResourceID:   nodeID,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"action": "rollback",
			// 回滚的目标状态是**停用**：把系统写入的规则清掉，让宿主机
			// 回到"没有任何本系统的防火墙"——那是唯一确定安全的状态。
			"enabled": false,
		},
	})
	if err != nil {
		return nil, api.Internal()
	}

	// **先确保策略行存在**，否则这条 UPDATE 命中 0 行而**静默什么都不做**：
	// 回滚看起来成功了（任务已入队、接口返回 200），而"最近回滚时刻"没有
	// 留下——那正是排查"为什么规则和我配的不一样"时第一个要看的东西。
	if _, err := s.loadPolicy(ctx, nodeID); err != nil {
		return nil, err
	}
	now := s.now()
	if err := s.db.WithContext(ctx).Model(&model.HostFirewallPolicy{}).
		Where("node_id = ?", nodeID).Updates(map[string]any{
		"enabled": false, "last_rollback_at": now,
	}).Error; err != nil {
		log.Printf("[hostfirewall] 记录回滚失败: %v", err)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "host_firewall",
		Action:  "host_firewall.rollback",
		Params:  map[string]any{"note": "紧急回滚：撤销本系统写入的全部宿主机规则"},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// Connections 返回宿主机当前的入站连接。**只读探测**。
func (s *Service) Connections(ctx context.Context, nodeID int64) ([]agent.HostConnection, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostConnections, NodeID: nodeID,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取当前连接")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	conns, _ := result.Data[agent.HostConnectionsKey].([]agent.HostConnection)
	return conns, nil
}

// CloseConnection 关闭一条连接。
//
// **它会切断正在进行的会话**——包括管理员自己那条。因此它不是普通操作，
// 而界面上必须把"这条连接属于谁"写清楚，否则用户很容易关掉自己。
func (s *Service) CloseConnection(
	ctx context.Context, nodeID int64, remoteAddr string,
	v authz.Viewer, operatorName, clientIP string,
) error {
	if strings.TrimSpace(remoteAddr) == "" {
		return api.InvalidParameter("必须指定要关闭的连接")
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostConnections, NodeID: nodeID,
		Params: map[string]any{"close": remoteAddr},
	})
	if err != nil {
		return api.Unavailable("节点不可达，连接未关闭")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "host_firewall",
		ResourceName: remoteAddr,
		Action:       "host_firewall.connection.close",
		Success:      true, ClientIP: clientIP,
	})
	return nil
}

// --- 内部 ---

func (s *Service) loadPolicy(ctx context.Context, nodeID int64) (*model.HostFirewallPolicy, error) {
	var policy model.HostFirewallPolicy
	err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&policy).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 没有就现建一份默认的：**默认不启用**。
		//
		// 这与"默认拒绝"不矛盾——默认拒绝是**启用之后**的行为。一上来就
		// 启用会让所有现网节点在下一次判定时立刻收紧，而那可能切断正在
		// 使用的连接。
		policy = model.HostFirewallPolicy{
			NodeID: nodeID, Enabled: false, DefaultAction: model.FirewallDeny,
		}
		if err := s.db.WithContext(ctx).Create(&policy).Error; err != nil {
			log.Printf("[hostfirewall] 创建默认策略失败: %v", err)
			return nil, api.Internal()
		}
		return &policy, nil
	}
	if err != nil {
		log.Printf("[hostfirewall] 查询策略失败: %v", err)
		return nil, api.Internal()
	}
	return &policy, nil
}

// fixedRules 合成两条**不在表里**的保护规则：面板端口与 SSH。
//
// 算出来而不是存进表：端口是配置项，改配置之后表里那条就过期了，而一条
// 过期的"保护规则"比没有更糟——它保护着一个不再监听的端口，而真正的端口
// 没有任何规则挡着。
func (s *Service) fixedRules(nodeID int64) []View {
	out := []View{{
		ID: 0, NodeID: nodeID, Action: model.FirewallAccept, Protocol: "tcp",
		PortStart: &s.panelPort, IsProtected: true, Fixed: true,
		FixedReason: "面板自身的监听端口——删掉它你会立刻连不上面板，而那时已经没有界面可以把它加回来",
		Describe:    fmt.Sprintf("tcp %d 来自任意来源（面板）", s.panelPort),
	}}
	for _, p := range s.sshPorts {
		port := p
		out = append(out, View{
			ID: 0, NodeID: nodeID, Action: model.FirewallAccept, Protocol: "tcp",
			PortStart: &port, IsProtected: true, Fixed: true,
			FixedReason: "SSH 端口——删掉它你就只剩进机房这一条路",
			Describe:    fmt.Sprintf("tcp %d 来自任意来源（SSH）", port),
		})
	}
	return out
}

func (s *Service) bumpVersion(ctx context.Context, nodeID int64) {
	if _, err := s.loadPolicy(ctx, nodeID); err != nil {
		return
	}
	if err := s.db.WithContext(ctx).Model(&model.HostFirewallPolicy{}).
		Where("node_id = ?", nodeID).
		Update("version", gorm.Expr("version + 1")).Error; err != nil {
		log.Printf("[hostfirewall] 递增版本失败: %v", err)
	}
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

// buildSpec 把规则转成下发的结构（顺序稳定）。
func buildSpec(rules []View) []map[string]any {
	out := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		out = append(out, map[string]any{
			"action": r.Action, "protocol": r.Protocol,
			"port_start": r.PortStart, "port_end": r.PortEnd,
			"source_cidr": r.SourceCIDR, "geoip_regions": r.GeoipRegions,
			// 保护规则必须一起下发：它们不在表里，但少了它们，
			// 应用之后管理员立刻连不上。
			"is_protected": r.IsProtected,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["describe"]) < fmt.Sprint(out[j]["describe"])
	})
	return out
}

func validatePorts(protocol string, start, end *int) error {
	if protocol == "icmp" && start != nil {
		return api.InvalidParameter("ICMP 不区分端口——填了端口会得到一条看起来有限制、实际没有的规则")
	}
	if start == nil && end != nil {
		return api.InvalidParameter("填了结束端口就必须填起始端口")
	}
	if start != nil && (*start < 1 || *start > 65535) {
		return api.InvalidParameter("端口必须在 1~65535 之间")
	}
	if end != nil && (*end < 1 || *end > 65535) {
		return api.InvalidParameter("端口必须在 1~65535 之间")
	}
	if start != nil && end != nil && *end < *start {
		return api.InvalidParameter("结束端口不能小于起始端口")
	}
	return nil
}

func normalizeCIDRs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func splitList(p *string) []string {
	if p == nil {
		return []string{}
	}
	return normalizeCIDRs(*p)
}

func hasUnapplied(rows []model.HostFirewallRule) bool {
	for i := range rows {
		if !rows[i].Applied {
			return true
		}
	}
	return false
}

func toView(r *model.HostFirewallRule) View {
	v := View{
		ID: r.ID, NodeID: r.NodeID, Action: r.Action, Protocol: r.Protocol,
		PortStart: r.PortStart, PortEnd: r.PortEnd,
		IsProtected: r.IsProtected, Applied: r.Applied,
		Describe: r.Describe(),
	}
	if r.SourceCIDR != nil {
		v.SourceCIDR = *r.SourceCIDR
	}
	if r.GeoipRegions != nil && *r.GeoipRegions != "" {
		v.GeoipRegions = strings.Split(*r.GeoipRegions, ",")
	}
	if r.Remark != nil {
		v.Remark = *r.Remark
	}
	return v
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
