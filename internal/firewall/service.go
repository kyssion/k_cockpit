// Package firewall 实现双层防火墙策略（F-4-11）。
//
// 双层是：**节点级基线**（firewall_policy + firewall_rule）作用在节点上所有
// 虚拟机；**虚拟机级覆盖**（firewall_vm_policy）在基线之上更严或替换区域与
// 白名单。
//
// 有一条贯穿本包的安全原则，它决定了下面几乎所有设计：
//
//	**防火墙最重要的性质不是「能挡住什么」，而是「不会把管理员挡在外面」。**
//
// 一次配错无法通过面板恢复——那时已经连不上了，只能上宿主机敲命令。因此：
//
//   - 保护规则（SSH、面板、转发放通）**服务端强制不可删改**；
//   - 白名单**优先于一切拒绝**，包括区域限制；
//   - 应用前**预检**会指出这次改动会不会切断管理通道，并要求显式确认；
//   - 紧急回滚**不需要任何确认**，一键关闭。
//
// 最后一条是有意的**不对称**：通往事故的路要设卡，从事故里出来的路不能设卡。
package firewall

import (
	"context"
	"errors"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Service 提供防火墙能力。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	audit *audit.Recorder
}

// NewService 构造防火墙服务。
func NewService(db *gorm.DB, client agent.Client, recorder *audit.Recorder) *Service {
	return &Service{db: db, agent: client, audit: recorder}
}

// PolicyView 是节点级策略的对外视图。
type PolicyView struct {
	NodeID        int64    `json:"node_id"`
	Enabled       bool     `json:"enabled"`
	DefaultAction string   `json:"default_action"`
	GeoipRegions  []string `json:"geoip_regions"`
	Whitelist     []string `json:"whitelist"`
	Version       int      `json:"version"`
	// AppliedAt 是最近一次成功下发到宿主机的时刻；为空表示从未下发过。
	AppliedAt string `json:"applied_at,omitempty"`
	// Pending 表示**有改动还没下发**。
	//
	// 判据取规则级的 applied 标志，而不是比较版本号：表结构里没有
	// 「已下发的版本」这一列，而 firewall_rule.applied 本来就是为此留的
	// ——它还能进一步指出**是哪几条**没生效，比一句「有改动」有用得多。
	Pending bool `json:"pending"`
	// UnappliedRules 是尚未下发的规则条数。
	UnappliedRules int `json:"unapplied_rules"`
}

// RuleView 是一条规则的对外视图。
type RuleView struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	Protocol    string `json:"protocol"`
	PortStart   *int   `json:"port_start,omitempty"`
	PortEnd     *int   `json:"port_end,omitempty"`
	SourceCIDR  string `json:"source_cidr,omitempty"`
	IsProtected bool   `json:"is_protected"`
	OrderNo     int    `json:"order_no"`
	Applied     bool   `json:"applied"`
	Remark      string `json:"remark,omitempty"`
}

// Effective 是一台虚拟机实际受到的约束。
type Effective struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// Source 说明这份约束来自哪一层（node / vm）。
	Source        string   `json:"source"`
	Enabled       bool     `json:"enabled"`
	DefaultAction string   `json:"default_action"`
	GeoipRegions  []string `json:"geoip_regions"`
	Whitelist     []string `json:"whitelist"`
	// Overridden 为 true 表示该虚拟机有自己的覆盖策略。
	Overridden bool       `json:"overridden"`
	Rules      []RuleView `json:"rules"`
	// Warnings 是这台机器当前配置里值得注意的地方。
	Warnings []string `json:"warnings,omitempty"`
}

// GetPolicy 返回节点的防火墙策略。
func (s *Service) GetPolicy(ctx context.Context, nodeID int64) (*PolicyView, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	rules, err := s.countUnapplied(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return toPolicyView(policy, rules), nil
}

// UpdatePolicyRequest 是更新节点级策略的请求。
type UpdatePolicyRequest struct {
	Enabled       *bool
	DefaultAction string
	GeoipRegions  []string
	Whitelist     []string
}

// UpdatePolicy 更新节点级策略。
//
// **只改配置，不下发**。下发是 Apply 的事——把两者分开是因为下发有真实的
// 网络影响，而改配置不该有。合成一步会让「我想先看看改完是什么样」变得
// 不可能。
func (s *Service) UpdatePolicy(
	ctx context.Context, nodeID int64, req UpdatePolicyRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*PolicyView, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	if req.DefaultAction != "" && !model.ValidFirewallAction(req.DefaultAction) {
		return nil, api.InvalidParameter("默认处置必须是 accept 或 deny")
	}
	whitelist, err := normalizeCIDRList(req.Whitelist, "白名单")
	if err != nil {
		return nil, err
	}

	updates := map[string]any{
		// version 每次改动自增：它是「预览 → 应用」之间的凭据，
		// 也是判断「宿主机上跑的是哪一版」的依据。
		"version": gorm.Expr("version + 1"),
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.DefaultAction != "" {
		updates["default_action"] = req.DefaultAction
	}
	if req.GeoipRegions != nil {
		updates["geoip_regions"] = strings.Join(req.GeoipRegions, ",")
	}
	if req.Whitelist != nil {
		updates["whitelist"] = strings.Join(whitelist, "\n")
	}

	before := toPolicyView(policy, 0)
	if err := s.db.WithContext(ctx).Model(&model.FirewallPolicy{}).
		Where("id = ?", policy.ID).Updates(updates).Error; err != nil {
		log.Printf("[firewall] 更新策略失败: %v", err)
		return nil, api.Internal()
	}

	updated, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "firewall_policy", ResourceID: policy.ID,
		ResourceName: "node:" + strconv.FormatInt(nodeID, 10),
		Action:       "firewall.policy.update",
		BeforeState:  map[string]any{"enabled": before.Enabled, "default_action": before.DefaultAction, "whitelist": before.Whitelist},
		AfterState:   map[string]any{"enabled": updated.Enabled, "default_action": updated.DefaultAction, "whitelist": updated.Whitelist},
		Success:      true, ClientIP: clientIP,
	})
	return toPolicyView(updated, 0), nil
}

// ListRules 返回节点级规则。
func (s *Service) ListRules(ctx context.Context, nodeID int64) ([]RuleView, error) {
	var rules []model.FirewallRule
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("is_protected DESC, order_no ASC, id ASC").
		Find(&rules).Error; err != nil {
		log.Printf("[firewall] 查询规则失败: %v", err)
		return nil, api.Internal()
	}
	views := make([]RuleView, 0, len(rules))
	for i := range rules {
		views = append(views, toRuleView(&rules[i]))
	}
	return views, nil
}

// RuleRequest 是新增或修改规则的请求。
type RuleRequest struct {
	Action     string
	Protocol   string
	PortStart  *int
	PortEnd    *int
	SourceCIDR string
	OrderNo    int
	Remark     string
}

// CreateRule 新增一条节点级规则。
func (s *Service) CreateRule(
	ctx context.Context, nodeID int64, req RuleRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*RuleView, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	rule, err := normalizeRule(nodeID, req)
	if err != nil {
		return nil, err
	}

	// 服务层去重，对应迁移里的 uniq_firewall_rule_dedup。
	//
	// 那条索引是表达式索引（coalesce 把可空列归一化），GORM 无法在模型上
	// 声明——见 model.FirewallRule 的说明。因此这里按同样的语义比对：
	// NULL 与 0 视为同一个值。
	if dup, err := s.duplicateOf(ctx, rule); err != nil {
		return nil, err
	} else if dup {
		return nil, api.Conflict("已存在完全相同的规则")
	}

	if err := s.db.WithContext(ctx).Create(rule).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("已存在完全相同的规则")
		}
		log.Printf("[firewall] 新增规则失败: %v", err)
		return nil, api.Internal()
	}

	// 规则变了，策略版本也要变——版本是「当前这套配置」的指纹，
	// 只把规则改动排除在外的话，预览与应用的校验会放过一次规则变更。
	if err := s.bumpVersion(ctx, policy.ID); err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "firewall_rule", ResourceID: rule.ID,
		ResourceName: describeRule(rule), Action: "firewall.rule.create",
		Params:  map[string]any{"action": rule.Action, "protocol": rule.Protocol, "source": req.SourceCIDR},
		Success: true, ClientIP: clientIP,
	})
	view := toRuleView(rule)
	return &view, nil
}

// DeleteRule 删除一条节点级规则。
//
// **保护规则拒绝删除**。这必须由服务端强制：一次「清理规则」的操作就能把
// SSH 或面板端口关掉，而那种事故**无法通过面板恢复**——那时已经连不上了。
// 让界面决定能不能删，等于把一个不可恢复的操作交给一次点击。
func (s *Service) DeleteRule(
	ctx context.Context, nodeID, ruleID int64, v authz.Viewer, operatorName, clientIP string,
) error {
	rule, err := s.loadRule(ctx, nodeID, ruleID)
	if err != nil {
		return err
	}
	if rule.IsProtected {
		return api.ValidationFailed(
			"「" + describeRule(rule) + "」是系统保护规则，不可删除；" +
				"它保护的是管理通道本身，删掉会让面板无法访问")
	}

	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Delete(&model.FirewallRule{}, rule.ID).Error; err != nil {
		log.Printf("[firewall] 删除规则失败: %v", err)
		return api.Internal()
	}
	if err := s.bumpVersion(ctx, policy.ID); err != nil {
		return err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "firewall_rule", ResourceID: rule.ID,
		ResourceName: describeRule(rule), Action: "firewall.rule.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// SetVMPolicy 设置一台虚拟机的覆盖策略。
func (s *Service) SetVMPolicy(
	ctx context.Context, vmID int64, action, whitelist string, regions []string,
	enabled bool, v authz.Viewer, operatorName, clientIP string,
) (*Effective, error) {
	vm, err := s.loadVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	if action == "" {
		action = model.FirewallAccept
	}
	if !model.ValidFirewallAction(action) {
		return nil, api.InvalidParameter("动作必须是 accept 或 deny")
	}
	list, err := normalizeCIDRList(splitLines(whitelist), "白名单")
	if err != nil {
		return nil, err
	}
	joined := strings.Join(list, "\n")
	zoneList := strings.Join(regions, ",")

	var existing model.FirewallVMPolicy
	err = s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		row := model.FirewallVMPolicy{
			VMID: vmID, Action: action, Enabled: enabled,
			Whitelist: &joined, GeoipRegions: &zoneList,
		}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			log.Printf("[firewall] 创建虚拟机策略失败: %v", err)
			return nil, api.Internal()
		}
	case err != nil:
		log.Printf("[firewall] 查询虚拟机策略失败: %v", err)
		return nil, api.Internal()
	default:
		if err := s.db.WithContext(ctx).Model(&model.FirewallVMPolicy{}).
			Where("id = ?", existing.ID).
			Updates(map[string]any{
				"action": action, "enabled": enabled,
				"whitelist": &joined, "geoip_regions": &zoneList,
			}).Error; err != nil {
			log.Printf("[firewall] 更新虚拟机策略失败: %v", err)
			return nil, api.Internal()
		}
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "firewall.vm_policy.set",
		Params:  map[string]any{"action": action, "enabled": enabled, "whitelist": list, "regions": regions},
		Success: true, ClientIP: clientIP,
	})

	return s.Effective(ctx, vmID)
}

// ClearVMPolicy 移除虚拟机级覆盖，回落到节点基线。
func (s *Service) ClearVMPolicy(
	ctx context.Context, vmID int64, v authz.Viewer, operatorName, clientIP string,
) (*Effective, error) {
	vm, err := s.loadVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	res := s.db.WithContext(ctx).Where("vm_id = ?", vmID).Delete(&model.FirewallVMPolicy{})
	if res.Error != nil {
		log.Printf("[firewall] 删除虚拟机策略失败: %v", res.Error)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "firewall.vm_policy.clear",
		Success: true, ClientIP: clientIP,
	})
	return s.Effective(ctx, vmID)
}

// Effective 汇总一台虚拟机实际受到的约束（双层合并的结果）。
func (s *Service) Effective(ctx context.Context, vmID int64) (*Effective, error) {
	vm, err := s.loadVM(ctx, vmID)
	if err != nil {
		return nil, err
	}
	policy, err := s.loadPolicy(ctx, vm.NodeID)
	if err != nil {
		return nil, err
	}
	rules, err := s.ListRules(ctx, vm.NodeID)
	if err != nil {
		return nil, err
	}

	out := &Effective{
		VMID: vm.ID, VMName: vm.Name, Source: "node",
		Enabled: policy.Enabled, DefaultAction: policy.DefaultAction,
		GeoipRegions: splitList(policy.GeoipRegions),
		Whitelist:    splitList(policy.Whitelist),
		Rules:        rules,
	}

	// 虚拟机级覆盖：**替换**而不是叠加。
	//
	// 叠加会让「这台机器到底受哪些约束」变成两个列表的并集，而用户排查时
	// 需要在两份配置之间来回对照。替换让这台机器的策略是自包含的——
	// 看这一条就够了。
	var override model.FirewallVMPolicy
	err = s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&override).Error
	switch {
	case err == nil && override.Enabled:
		out.Overridden = true
		out.Source = "vm"
		out.DefaultAction = override.Action
		// **无条件替换**，包括「覆盖层里没设的那一项」。
		//
		// 只在非 nil 时才替换是一个看起来更"保守"、实际更危险的实现：
		// 用户在覆盖层里清空区域限制，得到的却是「节点级的区域限制继续
		// 生效」——而这台机器看起来已经"有自己的策略"了，他不会想到
		// 还有一层在起作用。
		out.GeoipRegions = splitList(override.GeoipRegions)
		out.Whitelist = splitList(override.Whitelist)
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound):
		log.Printf("[firewall] 查询虚拟机策略失败: %v", err)
		return nil, api.Internal()
	}

	out.Warnings = warningsFor(out)
	return out, nil
}

// Precheck 检查一次待应用的改动会不会**切断管理通道**（F-4-11 的紧急回滚
// 之所以必要，正是因为这一步防不住所有情况）。
//
// 它返回警告而不是拒绝：有时「就是要挡掉这些」是用户的真实意图。但警告
// 必须在应用时被**显式确认**——否则它就只是一行没人看的黄字。
func (s *Service) Precheck(ctx context.Context, nodeID int64, fromIP string) ([]string, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	rules, err := s.ListRules(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	var warnings []string

	if !policy.Enabled {
		return []string{"防火墙当前是**关闭**状态，应用之后才会开始拦截流量"}, nil
	}

	// 1) 当前请求的来源会不会被挡掉。
	//
	// 这是最直接的一条：如果新配置不允许现在这个连接，那么点下「应用」
	// 的那一刻，用户就失去了继续操作的能力。
	if fromIP != "" && !allowedSource(policy.Whitelist, rules, fromIP) {
		warnings = append(warnings, "当前访问来源 "+fromIP+
			" 不在白名单里，且没有规则放行它——应用后你将无法访问面板")
	}

	// 2) 项目默认拒绝 + 白名单为空。
	//
	// 这是最危险的一种组合：默认拒绝意味着「不匹配就等于断」，而白名单
	// 为空意味着没有一条无条件的放行。规则里但凡漏了什么，就连不上了。
	if policy.DefaultAction == model.FirewallDeny && len(splitList(policy.Whitelist)) == 0 {
		warnings = append(warnings,
			"默认处置为拒绝且白名单为空：任何未被规则放行的来源都会被挡掉，"+
				"包括你自己的管理连接")
	}

	// 3) 保护规则缺失。
	//
	// 保护规则本该由服务端在端口转发开通等路径上自动生成；它不在了说明
	// 有人手工清理过，而清掉的恰是管理通道的放行。
	if !hasProtectedRule(rules) {
		warnings = append(warnings,
			"当前没有系统保护规则：管理通道的放行已缺失，建议先确认面板与 SSH "+
				"端口仍有规则放行")
	}

	// 4) 区域限制与白名单的关系。
	if len(splitList(policy.GeoipRegions)) > 0 && len(splitList(policy.Whitelist)) == 0 {
		warnings = append(warnings,
			"启用了区域限制但没有白名单：区域数据不总是准确，"+
				"若你的来源被误判你会连不上面板")
	}

	return warnings, nil
}

// ApplyResult 是应用结果。
type ApplyResult struct {
	Version  int      `json:"version"`
	Warnings []string `json:"warnings,omitempty"`
	// Applied 为 false 表示因为有未确认的警告而没有下发。
	Applied bool `json:"applied"`
}

// Apply 把节点级策略下发到宿主机。
//
// **同步调用节点，不经任务队列**。这与虚拟机的创建、快照等操作不同，
// 理由是三条：
//
//   - 写 iptables 规则是个快操作，排队带来的延迟远大于它本身；
//   - 管理员点下「应用」之后**立刻需要一个答复**——他要知道自己有没有
//     被关在门外，而排队会让这个答复晚到几十秒，那时他已经在怀疑是
//     不是网络断了；
//   - 最重要的是**紧急回滚**（见 Rollback）：如果下发要排队，一次回滚
//     会排在队列里几十个虚拟机创建之后，而那时管理员已经连不上面板了。
//
// 有警告时必须由调用方显式确认（`Acknowledge`）——**未确认时不下发、也不
// 报错**，而是把警告原样返回。报错会让界面把它显示成一次失败，而它实际
// 是一个需要用户做决定的岔路口。
func (s *Service) Apply(
	ctx context.Context, nodeID int64, acknowledge bool, fromIP string,
	v authz.Viewer, operatorName, clientIP string,
) (*ApplyResult, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	warnings, err := s.Precheck(ctx, nodeID, fromIP)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 && !acknowledge {
		return &ApplyResult{Version: policy.Version, Warnings: warnings, Applied: false}, nil
	}

	rules, err := s.ListRules(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpFirewallApply,
		NodeID: nodeID,
		Target: "node:" + strconv.FormatInt(nodeID, 10),
		Params: map[string]any{
			"enabled":        policy.Enabled,
			"default_action": policy.DefaultAction,
			"geoip_regions":  splitList(policy.GeoipRegions),
			"whitelist":      splitList(policy.Whitelist),
			"version":        policy.Version,
			"rules":          rules,
		},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，防火墙策略未下发")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	// 只有下发成功才记版本：节点失败时控制面认为策略已生效，用户按
	// 「已经生效」的前提去排查一条实际不存在的规则，方向从一开始就是错的。
	if err := s.db.WithContext(ctx).Model(&model.FirewallPolicy{}).
		Where("id = ?", policy.ID).
		Update("applied_at", time.Now()).Error; err != nil {
		log.Printf("[firewall] 回写应用版本失败 node=%d: %v", nodeID, err)
	}
	if err := s.db.WithContext(ctx).Model(&model.FirewallRule{}).
		Where("node_id = ?", nodeID).
		Update("applied", true).Error; err != nil {
		log.Printf("[firewall] 回写规则应用状态失败: %v", err)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "firewall_policy", ResourceID: policy.ID,
		ResourceName: "node:" + strconv.FormatInt(nodeID, 10),
		Action:       "firewall.apply",
		Params: map[string]any{
			"version": policy.Version, "enabled": policy.Enabled,
			"rule_count": len(rules), "acknowledged_warnings": warnings,
		},
		Success: true, ClientIP: clientIP,
	})

	return &ApplyResult{Version: policy.Version, Warnings: warnings, Applied: true}, nil
}

// Rollback 紧急关闭防火墙（F-4-11：紧急回滚）。
//
// **不做任何确认、不做预检、不需要二次验证**，一键关闭。
//
// 这个「不做」是有意的**不对称**：通往事故的路要设卡（Apply 要确认警告），
// 从事故里出来的路不能设卡。一个被自己配错的防火墙关在门外的管理员，此刻
// 唯一的诉求是「先让我进去」，而任何一道额外确认都会成为压垮他的那一步。
//
// 它同时把策略标记为「未应用」——宿主机上的规则还在，但控制面不再声称
// 两边一致。用户恢复访问之后可以重新下发。
func (s *Service) Rollback(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*ApplyResult, error) {
	policy, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpFirewallRollback,
		NodeID: nodeID,
		Target: "node:" + strconv.FormatInt(nodeID, 10),
		Params: map[string]any{"reason": "紧急回滚"},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法回滚")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Model(&model.FirewallPolicy{}).
		Where("id = ?", policy.ID).
		Updates(map[string]any{
			"enabled": false,
			"version": gorm.Expr("version + 1"),
			// 清掉应用时刻：宿主机上的规则虽然还在，但控制面不再声称两边
			// 一致——用户恢复访问之后需要重新下发。
			"applied_at": nil,
		}).Error; err != nil {
		log.Printf("[firewall] 回滚策略状态失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "firewall_policy", ResourceID: policy.ID,
		ResourceName: "node:" + strconv.FormatInt(nodeID, 10),
		Action:       "firewall.rollback",
		Success:      true, ClientIP: clientIP,
	})

	updated, err := s.loadPolicy(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return &ApplyResult{Version: updated.Version, Applied: true}, nil
}

// --- 内部 ---

// loadPolicy 读取（或在缺失时创建）节点策略。
func (s *Service) loadPolicy(ctx context.Context, nodeID int64) (*model.FirewallPolicy, error) {
	var policy model.FirewallPolicy
	err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&policy).Error
	if err == nil {
		return &policy, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("[firewall] 查询策略失败: %v", err)
		return nil, api.Internal()
	}

	// 首次访问时建一条**默认关闭**的策略。
	//
	// 默认关闭而不是默认开启：新建一条默认 deny 的策略会让所有未配规则的
	// 节点立刻开始拦截流量，包括面板自己——那是一次由「查看页面」触发的
	// 事故。
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	policy = model.FirewallPolicy{
		NodeID: nodeID, Enabled: false, DefaultAction: model.FirewallDeny,
	}
	if err := s.db.WithContext(ctx).Create(&policy).Error; err != nil {
		// 并发下可能已被别人建好，重新读一次。
		if e := s.db.WithContext(ctx).Where("node_id = ?", nodeID).First(&policy).Error; e != nil {
			log.Printf("[firewall] 创建策略失败: %v / %v", err, e)
			return nil, api.Internal()
		}
	}
	return &policy, nil
}

func (s *Service) loadRule(ctx context.Context, nodeID, ruleID int64) (*model.FirewallRule, error) {
	var rule model.FirewallRule
	if err := s.db.WithContext(ctx).
		Where("id = ? AND node_id = ?", ruleID, nodeID).First(&rule).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("规则不存在")
		}
		log.Printf("[firewall] 查询规则失败: %v", err)
		return nil, api.Internal()
	}
	return &rule, nil
}

func (s *Service) loadVM(ctx context.Context, id int64) (*model.VM, error) {
	var vm model.VM
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&vm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("虚拟机不存在")
		}
		log.Printf("[firewall] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	return &vm, nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	if err := s.db.WithContext(ctx).
		Select("id", "enroll_state").Where("id = ?", nodeID).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		log.Printf("[firewall] 查询节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("节点尚未接入")
	}
	return nil
}

// bumpVersion 让策略版本自增。
//
// 规则**单独增删**也要走这一步：版本是「当前这套配置」的指纹，只把策略
// 字段的改动算进去的话，一次规则变更会被版本校验放过。
func (s *Service) bumpVersion(ctx context.Context, policyID int64) error {
	if err := s.db.WithContext(ctx).Model(&model.FirewallPolicy{}).
		Where("id = ?", policyID).
		Update("version", gorm.Expr("version + 1")).Error; err != nil {
		log.Printf("[firewall] 递增版本失败: %v", err)
		return api.Internal()
	}
	return nil
}

func (s *Service) countUnapplied(ctx context.Context, nodeID int64) (int, error) {
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.FirewallRule{}).
		Where("node_id = ? AND applied = ?", nodeID, false).Count(&n).Error; err != nil {
		log.Printf("[firewall] 统计未应用规则失败: %v", err)
		return 0, api.Internal()
	}
	return int(n), nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

// duplicateOf 按 uniq_firewall_rule_dedup 的语义判断规则是否已存在。
//
// 比对口径必须与那条索引一致：可空列**归一化之后再比**（NULL 与 0 等价），
// 否则服务层会放过一条数据库会拦下的规则——两边对「什么算重复」给出不同
// 答案，而测试库因为没有那条索引，永远看不到这个差异。
func (s *Service) duplicateOf(ctx context.Context, rule *model.FirewallRule) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&model.FirewallRule{}).
		Where("node_id = ? AND action = ? AND protocol = ?", rule.NodeID, rule.Action, rule.Protocol).
		Where("COALESCE(port_start, 0) = ? AND COALESCE(port_end, 0) = ?",
			derefInt(rule.PortStart), derefInt(rule.PortEnd)).
		Where("COALESCE(source_cidr, '') = ?", derefStr(rule.SourceCIDR)).
		Count(&n).Error
	if err != nil {
		log.Printf("[firewall] 查重失败: %v", err)
		return false, api.Internal()
	}
	return n > 0, nil
}

// normalizeRule 校验并补全一条规则。
func normalizeRule(nodeID int64, req RuleRequest) (*model.FirewallRule, error) {
	if !model.ValidFirewallAction(req.Action) {
		return nil, api.InvalidParameter("动作必须是 accept 或 deny")
	}
	protocol := req.Protocol
	if protocol == "" {
		protocol = model.ProtocolTCP
	}
	if !model.ValidFirewallProtocol(protocol) {
		return nil, api.InvalidParameter("不支持的协议：" + protocol)
	}

	portStart, portEnd := req.PortStart, req.PortEnd
	if !model.ProtocolUsesPorts(protocol) {
		if portStart != nil || portEnd != nil {
			return nil, api.ValidationFailed(
				"协议为 " + protocol + " 时不能指定端口——填了会得到一条看起来有限制、" +
					"实际没有的规则")
		}
	} else if portStart != nil {
		if portEnd == nil {
			portEnd = portStart
		}
		if *portStart < 1 || *portStart > 65535 || *portEnd < 1 || *portEnd > 65535 {
			return nil, api.InvalidParameter("端口范围必须在 1-65535 之间")
		}
		if *portEnd < *portStart {
			return nil, api.InvalidParameter("结束端口不能小于起始端口")
		}
	}

	source := strings.TrimSpace(req.SourceCIDR)
	var sourcePtr *string
	if source != "" {
		normalized, err := normalizeCIDR(source)
		if err != nil {
			return nil, err
		}
		sourcePtr = &normalized
	}

	order := req.OrderNo
	if order <= 0 {
		order = 100
	}
	rule := &model.FirewallRule{
		NodeID: nodeID, Action: req.Action, Protocol: protocol,
		PortStart: portStart, PortEnd: portEnd, SourceCIDR: sourcePtr,
		OrderNo: order,
	}
	if req.Remark != "" {
		rule.Remark = &req.Remark
	}
	return rule, nil
}

// normalizeCIDR 校验并规范化一个 CIDR 或单地址。
func normalizeCIDR(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", api.InvalidParameter("来源不能为空")
	}
	if ip := net.ParseIP(value); ip != nil {
		// 单个地址补上 /32 或 /128：留着裸地址会让宿主机上的规则生成逻辑
		// 需要判断两种写法，而两种写法在比较时又互不相等。
		if ip.To4() != nil {
			return value + "/32", nil
		}
		return value + "/128", nil
	}
	if _, _, err := net.ParseCIDR(value); err != nil {
		return "", api.InvalidParameter("来源必须是 IP 或 CIDR，例如 203.0.113.7 或 10.0.0.0/8")
	}
	return value, nil
}

func normalizeCIDRList(list []string, label string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, raw := range list {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		value, err := normalizeCIDR(raw)
		if err != nil {
			return nil, api.InvalidParameter(label + "中有不合法的值：" + raw)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

// allowedSource 判断某个来源是否会被放行（用于预检）。
func allowedSource(whitelist *string, rules []RuleView, ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return true
	}
	// 白名单优先：它在任何规则之前生效。
	for _, cidr := range splitList(whitelist) {
		if containsIP(cidr, parsed) {
			return true
		}
	}
	for i := range rules {
		if rules[i].Action != model.FirewallAccept {
			continue
		}
		if rules[i].SourceCIDR == "" {
			return true
		}
		if containsIP(rules[i].SourceCIDR, parsed) {
			return true
		}
	}
	return false
}

func containsIP(cidr string, ip net.IP) bool {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return network.Contains(ip)
}

func hasProtectedRule(rules []RuleView) bool {
	for i := range rules {
		if rules[i].IsProtected {
			return true
		}
	}
	return false
}

// warningsFor 汇总一台虚拟机当前配置里值得注意的地方。
func warningsFor(e *Effective) []string {
	var out []string
	if !e.Enabled {
		return nil
	}
	if e.DefaultAction == model.FirewallDeny && len(e.Whitelist) == 0 {
		out = append(out,
			"默认处置为拒绝且白名单为空：任何未被放行的来源都会被挡掉")
	}
	if !hasProtectedRule(e.Rules) {
		out = append(out,
			"该节点上没有系统保护规则：管理通道的放行可能已缺失")
	}
	return out
}

// describeRule 把规则渲染成可读的一行，用于审计与拒绝文案。
func describeRule(r *model.FirewallRule) string {
	var b strings.Builder
	b.WriteString(r.Action)
	b.WriteString(" ")
	b.WriteString(r.Protocol)
	if r.PortStart != nil {
		b.WriteString(":" + strconv.Itoa(*r.PortStart))
		if r.PortEnd != nil && *r.PortEnd != *r.PortStart {
			b.WriteString("-" + strconv.Itoa(*r.PortEnd))
		}
	}
	b.WriteString(" from ")
	if r.SourceCIDR != nil {
		b.WriteString(*r.SourceCIDR)
	} else {
		b.WriteString("any")
	}
	return b.String()
}

func toPolicyView(p *model.FirewallPolicy, unapplied int) *PolicyView {
	view := &PolicyView{
		NodeID: p.NodeID, Enabled: p.Enabled, DefaultAction: p.DefaultAction,
		GeoipRegions:   splitList(p.GeoipRegions),
		Whitelist:      splitList(p.Whitelist),
		Version:        p.Version,
		UnappliedRules: unapplied,
	}
	if p.AppliedAt != nil {
		view.AppliedAt = p.AppliedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	view.Pending = unapplied > 0
	return view
}

func toRuleView(r *model.FirewallRule) RuleView {
	view := RuleView{
		ID: r.ID, Action: r.Action, Protocol: r.Protocol,
		PortStart: r.PortStart, PortEnd: r.PortEnd,
		IsProtected: r.IsProtected, OrderNo: r.OrderNo, Applied: r.Applied,
	}
	if r.SourceCIDR != nil {
		view.SourceCIDR = *r.SourceCIDR
	}
	if r.Remark != nil {
		view.Remark = *r.Remark
	}
	return view
}

// splitList 把逗号或换行分隔的一列拆开。
func splitList(raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return []string{}
	}
	return splitLines(*raw)
}

func splitLines(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
