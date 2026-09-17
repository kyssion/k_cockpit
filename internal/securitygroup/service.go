// Package securitygroup 实现安全组与其叠加生效（F-4-03 / F-4-04）。
//
// 三件事贯穿本包：
//
//  1. **组内只有允许规则**（表结构里没有 action 列）。安全组是叠加生效的
//     ——生效规则是各组的并集。在并集模型里「拒绝」没法定义：A 组拒绝 22、
//     B 组允许 22，合并后通不通取决于谁先算，而那是用户看不见的实现细节。
//
//  2. **默认拒绝由组的整体语义给出**，不由显式拒绝规则给出。要「只放行
//     特定来源」，做法是只写那几条允许规则。
//
//  3. **预览与应用之间绑定版本**（F-4-04）。用户在预览上做出的判断，必须
//     对应他确认时的那套规则——中间被别人改过的话，他确认的就不再是他看到
//     的东西了。
package securitygroup

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Service 提供安全组能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
}

// NewService 构造安全组服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder}
}

// GroupView 是一个安全组的对外视图。
type GroupView struct {
	ID        int64  `json:"id"`
	NodeID    int64  `json:"node_id"`
	OwnerID   *int64 `json:"owner_id,omitempty"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
	Remark    string `json:"remark,omitempty"`
	// RuleCount 规则条数。列表页要显示它，逐行查会变成 N 次往返。
	RuleCount int `json:"rule_count"`
	// AttachedCount 挂载它的网口数。
	//
	// 删除前要告诉用户这个数字：删掉一个正在被使用的组会**静默地**放开
	// 一批机器的流量，而那是一次安全性的收缩方向相反的变更。
	AttachedCount int    `json:"attached_count"`
	CreatedAt     string `json:"created_at"`
}

// RuleView 是一条规则的对外视图。
type RuleView struct {
	ID            int64  `json:"id"`
	GroupID       int64  `json:"group_id"`
	Direction     string `json:"direction"`
	Protocol      string `json:"protocol"`
	PortStart     *int   `json:"port_start,omitempty"`
	PortEnd       *int   `json:"port_end,omitempty"`
	TargetType    string `json:"target_type"`
	TargetValue   string `json:"target_value,omitempty"`
	AddressFamily string `json:"address_family"`
	Priority      int    `json:"priority"`
	Remark        string `json:"remark,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// EffectiveRule 是汇总后的**一条生效规则**。
type EffectiveRule struct {
	Direction     string `json:"direction"`
	Protocol      string `json:"protocol"`
	PortStart     *int   `json:"port_start,omitempty"`
	PortEnd       *int   `json:"port_end,omitempty"`
	TargetType    string `json:"target_type"`
	TargetValue   string `json:"target_value,omitempty"`
	AddressFamily string `json:"address_family"`
	// Sources 是贡献了这条规则的组名。
	//
	// **必须带出来**：叠加之后的规则表如果只显示合并结果，用户就不知道某条
	// 规则该去哪儿改——他会在 5 个组里逐个翻。带上来源之后，「这条允许 22
	// 端口的规则来自『Web 层』」一眼可见。
	Sources []string `json:"sources"`
}

// Preview 是生效规则的一次快照（f-4-04）。
type Preview struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// Version 是这份快照的指纹。应用时必须带回来（见 Apply）。
	Version string `json:"version"`
	// Groups 是参与叠加的组（含主组与附加组）。
	Groups []string `json:"groups"`
	// Rules 是去重后的生效规则。
	Rules []EffectiveRule `json:"rules"`
	// Warnings 是值得用户在应用前先看一眼的问题。
	Warnings []string `json:"warnings,omitempty"`
	// Interfaces 是参与计算的网口数量。
	InterfaceCount int `json:"interface_count"`
}

// ListGroups 返回节点上的安全组。
func (s *Service) ListGroups(ctx context.Context, nodeID int64, v authz.Viewer) ([]GroupView, error) {
	query := s.db.WithContext(ctx).Model(&model.SecurityGroup{}).Order("is_default DESC, name ASC")
	if nodeID > 0 {
		query = query.Where("node_id = ?", nodeID)
	}
	// 租户只看自己的组与管理员的预置组；管理员看全部。
	if !v.IsAdmin {
		query = query.Where("owner_id IS NULL OR owner_id = ?", v.UserID)
	}

	var groups []model.SecurityGroup
	if err := query.Find(&groups).Error; err != nil {
		log.Printf("[securitygroup] 查询安全组失败: %v", err)
		return nil, api.Internal()
	}
	if len(groups) == 0 {
		return []GroupView{}, nil
	}

	ids := make([]int64, 0, len(groups))
	for i := range groups {
		ids = append(ids, groups[i].ID)
	}

	// 规则数与挂载数**一次查完**：逐个查会把一次列表请求变成上百次往返。
	ruleCounts, err := s.countBy(ctx, &model.SecurityGroupRule{}, "group_id", ids)
	if err != nil {
		return nil, err
	}
	attachCounts, err := s.countBy(ctx, &model.InterfaceSecurityGroup{}, "group_id", ids)
	if err != nil {
		return nil, err
	}
	// 主组挂在 vm_interface 上，单独统计一次。
	var primaryCounts []struct {
		SecurityGroupID int64
		N               int
	}
	if err := s.db.WithContext(ctx).Model(&model.VMInterface{}).
		Select("security_group_id, count(*) AS n").
		Where("security_group_id IN ?", ids).
		Group("security_group_id").Scan(&primaryCounts).Error; err != nil {
		log.Printf("[securitygroup] 统计主组挂载数失败: %v", err)
		return nil, api.Internal()
	}
	for _, row := range primaryCounts {
		attachCounts[row.SecurityGroupID] += row.N
	}

	views := make([]GroupView, 0, len(groups))
	for i := range groups {
		views = append(views, toGroupView(&groups[i], ruleCounts[groups[i].ID], attachCounts[groups[i].ID]))
	}
	return views, nil
}

// CreateGroupRequest 是创建安全组的请求。
type CreateGroupRequest struct {
	NodeID int64
	Name   string
	Remark string
	// OwnerID 为空表示创建的是系统预置组（仅管理员可指定）。
	OwnerID *int64
}

// CreateGroup 创建安全组。
func (s *Service) CreateGroup(
	ctx context.Context, req CreateGroupRequest, v authz.Viewer, operatorName, clientIP string,
) (*GroupView, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, api.InvalidParameter("必须填写安全组名称")
	}
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}

	owner := req.OwnerID
	if owner == nil && !v.IsAdmin {
		// 租户创建的组归自己；预置组只能由管理员创建。
		id := v.UserID
		owner = &id
	}

	group := model.SecurityGroup{NodeID: req.NodeID, Name: name, OwnerID: owner}
	if req.Remark != "" {
		group.Remark = &req.Remark
	}
	if err := s.db.WithContext(ctx).Create(&group).Error; err != nil {
		// 名字冲突要说清「是哪个范围内的唯一」——节点内唯一而跨节点可以重名，
		// 不说明的话用户会以为是自己之前建过。
		if isDuplicateKey(err) {
			return nil, api.Conflict("该节点下已有同名安全组")
		}
		log.Printf("[securitygroup] 创建安全组失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "security_group", ResourceID: group.ID,
		ResourceName: name, Action: "security_group.create",
		Success: true, ClientIP: clientIP,
	})

	view := toGroupView(&group, 0, 0)
	return &view, nil
}

// DeleteGroup 删除安全组。
//
// **仍被网口挂载时拒绝**，并给出挂载数量。删掉一个正在被使用的组会**静默地**
// 放开一批机器的流量——那是一次方向与用户预期相反的变更，而且不报任何错。
func (s *Service) DeleteGroup(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	group, err := s.loadGroup(ctx, id, v)
	if err != nil {
		return err
	}
	if group.IsDefault {
		return api.ValidationFailed("默认安全组不可删除——新建网口会依赖它")
	}

	attached, err := s.attachedCount(ctx, id)
	if err != nil {
		return err
	}
	if attached > 0 {
		return api.Conflict(
			"该安全组仍被 " + strconv.Itoa(attached) + " 个网口使用，请先解除挂载；" +
				"直接删除会静默放开这些机器的流量")
	}

	if err := s.db.WithContext(ctx).Delete(&model.SecurityGroup{}, id).Error; err != nil {
		log.Printf("[securitygroup] 删除安全组失败: %v", err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: group.NodeID, ResourceType: "security_group", ResourceID: id,
		ResourceName: group.Name, Action: "security_group.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// ListRules 返回一个组的规则。
func (s *Service) ListRules(ctx context.Context, groupID int64, v authz.Viewer) ([]RuleView, error) {
	if _, err := s.loadGroup(ctx, groupID, v); err != nil {
		return nil, err
	}

	var rules []model.SecurityGroupRule
	if err := s.db.WithContext(ctx).
		Where("group_id = ?", groupID).
		// 排序按方向 + 优先级 + id：优先级相同的规则（默认全是 100）
		// 需要一个稳定的次序，否则同一份数据每次刷新顺序都可能不同，
		// 而一条顺序会变的规则表在读的时候让人怀疑自己看错了。
		Order("direction ASC, priority ASC, id ASC").
		Find(&rules).Error; err != nil {
		log.Printf("[securitygroup] 查询规则失败: %v", err)
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
	Direction   string
	Protocol    string
	PortStart   *int
	PortEnd     *int
	TargetType  string
	TargetValue string
	Priority    int
	Remark      string
}

// CreateRule 在组内新增一条允许规则。
func (s *Service) CreateRule(
	ctx context.Context, groupID int64, req RuleRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*RuleView, error) {
	group, err := s.loadGroup(ctx, groupID, v)
	if err != nil {
		return nil, err
	}
	normalized, err := s.normalizeRule(ctx, group, req)
	if err != nil {
		return nil, err
	}

	if err := s.db.WithContext(ctx).Create(normalized).Error; err != nil {
		log.Printf("[securitygroup] 创建规则失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: group.NodeID, ResourceType: "security_group", ResourceID: groupID,
		ResourceName: group.Name, Action: "security_group.rule.create",
		Params: map[string]any{
			"direction": normalized.Direction, "protocol": normalized.Protocol,
			"port_start": normalized.PortStart, "port_end": normalized.PortEnd,
			"target_type": normalized.TargetType, "target_value": normalized.TargetValue,
		},
		Success: true, ClientIP: clientIP,
	})

	view := toRuleView(normalized)
	return &view, nil
}

// UpdateRule 修改一条规则。
//
// **用修改而不是删旧建新**：重建会得到新 id，而历史审计里引用的是旧 id；
// 更重要的是「这条规则被改过」与「这条规则被删掉、又加了一条」是两件不同
// 的事，事后追查一个放行了不该放行的端口时，前者能直接回答「谁在什么时候
// 把它从 443 改成了 22」，后者只剩下两条互不相干的记录。
func (s *Service) UpdateRule(
	ctx context.Context, groupID, ruleID int64, req RuleRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*RuleView, error) {
	group, err := s.loadGroup(ctx, groupID, v)
	if err != nil {
		return nil, err
	}

	var existing model.SecurityGroupRule
	if err := s.db.WithContext(ctx).
		Where("id = ? AND group_id = ?", ruleID, groupID).
		First(&existing).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("规则不存在")
		}
		log.Printf("[securitygroup] 查询规则失败: %v", err)
		return nil, api.Internal()
	}

	normalized, err := s.normalizeRule(ctx, group, req)
	if err != nil {
		return nil, err
	}
	normalized.ID = existing.ID

	if err := s.db.WithContext(ctx).Model(&model.SecurityGroupRule{}).
		Where("id = ?", existing.ID).
		Updates(map[string]any{
			"direction": normalized.Direction, "protocol": normalized.Protocol,
			"port_start": normalized.PortStart, "port_end": normalized.PortEnd,
			"target_type": normalized.TargetType, "target_value": normalized.TargetValue,
			"address_family": normalized.AddressFamily,
			"priority":       normalized.Priority, "remark": normalized.Remark,
		}).Error; err != nil {
		log.Printf("[securitygroup] 更新规则失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: group.NodeID, ResourceType: "security_group", ResourceID: groupID,
		ResourceName: group.Name, Action: "security_group.rule.update",
		BeforeState: map[string]any{
			"direction": existing.Direction, "protocol": existing.Protocol,
			"port_start": existing.PortStart, "port_end": existing.PortEnd,
		},
		AfterState: map[string]any{
			"direction": normalized.Direction, "protocol": normalized.Protocol,
			"port_start": normalized.PortStart, "port_end": normalized.PortEnd,
		},
		Success: true, ClientIP: clientIP,
	})

	view := toRuleView(normalized)
	return &view, nil
}

// DeleteRule 删除一条规则。
func (s *Service) DeleteRule(
	ctx context.Context, groupID, ruleID int64, v authz.Viewer, operatorName, clientIP string,
) error {
	group, err := s.loadGroup(ctx, groupID, v)
	if err != nil {
		return err
	}

	res := s.db.WithContext(ctx).
		Where("id = ? AND group_id = ?", ruleID, groupID).
		Delete(&model.SecurityGroupRule{})
	if res.Error != nil {
		log.Printf("[securitygroup] 删除规则失败: %v", res.Error)
		return api.Internal()
	}
	if res.RowsAffected == 0 {
		return api.NotFound("规则不存在")
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: group.NodeID, ResourceType: "security_group", ResourceID: groupID,
		ResourceName: group.Name, Action: "security_group.rule.delete",
		Params:  map[string]any{"rule_id": ruleID},
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Attach 把安全组挂到网口上（附加组）。
func (s *Service) Attach(
	ctx context.Context, interfaceID, groupID int64, v authz.Viewer, operatorName, clientIP string,
) error {
	iface, err := s.loadInterface(ctx, interfaceID, v)
	if err != nil {
		return err
	}
	group, err := s.loadGroup(ctx, groupID, v)
	if err != nil {
		return err
	}
	if iface.NodeID != group.NodeID {
		return api.ValidationFailed("安全组与网口不在同一节点上")
	}

	// 已经是主组时不必再挂一次：生效规则取并集，重复挂载不会改变结果，
	// 却会让界面上出现两个相同的组标签。
	if iface.SecurityGroupID != nil && *iface.SecurityGroupID == groupID {
		return api.ValidationFailed("该安全组已经是这个网口的主组")
	}

	link := model.InterfaceSecurityGroup{InterfaceID: interfaceID, GroupID: groupID}
	if err := s.db.WithContext(ctx).Create(&link).Error; err != nil {
		if isDuplicateKey(err) {
			return api.Conflict("该安全组已挂载到这个网口")
		}
		log.Printf("[securitygroup] 挂载安全组失败: %v", err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: group.NodeID, ResourceType: "security_group", ResourceID: groupID,
		ResourceName: group.Name, Action: "security_group.attach",
		Params:  map[string]any{"interface_id": interfaceID},
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Detach 解除附加组的挂载。
//
// **主组不能通过这里解除**：主组挂在 vm_interface.security_group_id 上，
// 解除它等于把网口变成「没有安全组」，而那与「不限制」不是一回事——这里是
// 最容易造成误解的地方，因此单独拒绝并说明该怎么改。
func (s *Service) Detach(
	ctx context.Context, interfaceID, groupID int64, v authz.Viewer, operatorName, clientIP string,
) error {
	iface, err := s.loadInterface(ctx, interfaceID, v)
	if err != nil {
		return err
	}
	if iface.SecurityGroupID != nil && *iface.SecurityGroupID == groupID {
		return api.ValidationFailed(
			"这是该网口的主组，请在网口设置里更换安全组，而不是解除挂载")
	}
	group, err := s.loadGroup(ctx, groupID, v)
	if err != nil {
		return err
	}

	res := s.db.WithContext(ctx).
		Where("interface_id = ? AND group_id = ?", interfaceID, groupID).
		Delete(&model.InterfaceSecurityGroup{})
	if res.Error != nil {
		log.Printf("[securitygroup] 解除挂载失败: %v", res.Error)
		return api.Internal()
	}
	if res.RowsAffected == 0 {
		return api.NotFound("该安全组未挂载到这个网口")
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: group.NodeID, ResourceType: "security_group", ResourceID: groupID,
		ResourceName: group.Name, Action: "security_group.detach",
		Params:  map[string]any{"interface_id": interfaceID},
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Effective 汇总一台虚拟机的**生效规则**（f-4-03：多组叠加）。
//
// 步骤：找出它的所有网口 → 每个网口的参与组（主组 + 附加组）→ 收集所有
// 规则 → 去重，并记下每条规则来自哪些组。
//
// 跨网口也合并：一台机器有两个网口、各挂一个组时，从它对外的角度看，
// 生效的就是两组合起来的结果。按网口分开呈现会把「这台机器到底放行了什么」
// 这个问题拆成两份答案，而用户问的是一台机器。
func (s *Service) Effective(ctx context.Context, vmID int64, v authz.Viewer) (*Preview, error) {
	vm, err := s.loadVM(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	var ifaces []model.VMInterface
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).Order("\"order\" ASC").Find(&ifaces).Error; err != nil {
		log.Printf("[securitygroup] 查询网口失败: %v", err)
		return nil, api.Internal()
	}

	ifaceIDs := make([]int64, 0, len(ifaces))
	groupIDs := make([]int64, 0, len(ifaces))
	seen := map[int64]bool{}
	for i := range ifaces {
		ifaceIDs = append(ifaceIDs, ifaces[i].ID)
		if ifaces[i].SecurityGroupID != nil && !seen[*ifaces[i].SecurityGroupID] {
			seen[*ifaces[i].SecurityGroupID] = true
			groupIDs = append(groupIDs, *ifaces[i].SecurityGroupID)
		}
	}

	// 附加组。
	if len(ifaceIDs) > 0 {
		var links []model.InterfaceSecurityGroup
		if err := s.db.WithContext(ctx).
			Where("interface_id IN ?", ifaceIDs).Find(&links).Error; err != nil {
			log.Printf("[securitygroup] 查询附加组失败: %v", err)
			return nil, api.Internal()
		}
		for i := range links {
			if !seen[links[i].GroupID] {
				seen[links[i].GroupID] = true
				groupIDs = append(groupIDs, links[i].GroupID)
			}
		}
	}

	preview := &Preview{VMID: vm.ID, VMName: vm.Name, InterfaceCount: len(ifaces)}

	if len(groupIDs) == 0 {
		// 没有任何安全组时不返回空规则表——那看起来像「什么都不放行」，
		// 而实际含义取决于宿主机的默认策略。让界面能明确显示这个状态。
		preview.Groups = []string{}
		preview.Rules = []EffectiveRule{}
		preview.Warnings = []string{"该虚拟机没有挂载任何安全组，它当前不受安全组限制"}
		preview.Version = versionOf(nil, nil)
		return preview, nil
	}

	var groups []model.SecurityGroup
	if err := s.db.WithContext(ctx).
		Where("id IN ?", groupIDs).Order("id ASC").Find(&groups).Error; err != nil {
		log.Printf("[securitygroup] 查询安全组失败: %v", err)
		return nil, api.Internal()
	}
	names := make(map[int64]string, len(groups))
	for i := range groups {
		names[groups[i].ID] = groups[i].Name
		preview.Groups = append(preview.Groups, groups[i].Name)
	}

	var rules []model.SecurityGroupRule
	if err := s.db.WithContext(ctx).
		Where("group_id IN ?", groupIDs).
		Order("direction ASC, priority ASC, id ASC").Find(&rules).Error; err != nil {
		log.Printf("[securitygroup] 查询规则失败: %v", err)
		return nil, api.Internal()
	}

	preview.Rules = merge(rules, names)
	if len(preview.Rules) == 0 {
		preview.Warnings = append(preview.Warnings,
			"已挂载的安全组里一条规则都没有——该虚拟机不会收到任何放行")
	}

	// 规则里指向了别的组时，那些组也会被展开生效，但用户看不到它们的名字。
	if deps := referencedGroups(rules, names); len(deps) > 0 {
		preview.Warnings = append(preview.Warnings,
			"以下安全组被规则引用而间接生效，它们不在本机挂载列表里："+strings.Join(deps, "、"))
	}

	preview.Version = versionOf(groups, rules)
	return preview, nil
}

// Apply 把生效规则下发到节点（f-4-04：确认后再应用）。
//
// **version 必须匹配**。这是这个接口存在的主要理由：
//
// 用户看到预览、判断「这些规则没问题」，然后点确认。这中间可能有几十秒，
// 而在这段时间里别人完全可能改了其中一个组。如果不校验版本，用户确认的
// 就不再是他看到的那套规则了——他批准了 A，落下去的是 B。这类问题不会
// 报错，只会让某天出现一条谁也想不起来什么时候加的放行规则。
//
// 版本不匹配时返回冲突而**不是**自动用最新规则继续：自动继续等于替用户
// 批准了他没看过的东西，那正是要防的事。
func (s *Service) Apply(
	ctx context.Context, vmID int64, version string, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	preview, err := s.Effective(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if version == "" {
		return nil, api.InvalidParameter("必须携带预览版本号——应用前请先生成预览")
	}
	if preview.Version != version {
		return nil, api.Conflict(
			"生效规则在预览之后发生了变化，请重新预览后再确认；" +
				"直接应用会下发一套你没有看过的规则")
	}

	vm, err := s.loadVM(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskSecurityGroupApply,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id": vm.ID, "vm_name": vm.Name,
			"version": version, "rules": preview.Rules, "groups": preview.Groups,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "security_group.apply",
		Params: map[string]any{
			"task_id": t.ID, "version": version,
			"rule_count": len(preview.Rules), "groups": preview.Groups,
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// --- 内部 ---

// normalizeRule 校验并补全一条规则。
func (s *Service) normalizeRule(
	ctx context.Context, group *model.SecurityGroup, req RuleRequest,
) (*model.SecurityGroupRule, error) {
	direction := req.Direction
	if direction != model.DirectionIngress && direction != model.DirectionEgress {
		return nil, api.InvalidParameter("方向必须是 ingress（入站）或 egress（出站）")
	}
	protocol := req.Protocol
	if protocol == "" {
		protocol = model.ProtocolTCP
	}
	if !model.ValidProtocol(protocol) {
		return nil, api.InvalidParameter("不支持的协议：" + protocol)
	}

	portStart, portEnd := req.PortStart, req.PortEnd
	if !model.ProtocolUsesPorts(protocol) {
		// ICMP 没有端口概念，all 覆盖全部协议因而也不能限定端口。允许填端口
		// 会生成一条**看起来有约束、实际没有**的规则——用户以为只放行了
		// 22 端口，实际整个协议都通着，而这种偏差不会以任何形式报错。
		if portStart != nil || portEnd != nil {
			return nil, api.ValidationFailed(
				"协议为 " + protocol + " 时不能指定端口——" +
					"ICMP 没有端口概念，「全部」协议无法限定端口；" +
					"填了会得到一条看起来有限制、实际没有的规则")
		}
	} else {
		if portStart == nil {
			return nil, api.InvalidParameter("必须填写起始端口，或改用「全部」协议")
		}
		if portEnd == nil {
			// 单端口时 end 与 start 相等，而不是留空——「等于」与「不限」
			// 是两种不同的意思，让它们长得不一样才能一眼分辨。
			portEnd = portStart
		}
		if *portStart < 1 || *portStart > 65535 || *portEnd < 1 || *portEnd > 65535 {
			return nil, api.InvalidParameter("端口范围必须在 1-65535 之间")
		}
		if *portEnd < *portStart {
			return nil, api.InvalidParameter("结束端口不能小于起始端口")
		}
	}

	targetType := req.TargetType
	if targetType == "" {
		targetType = model.TargetCIDR
	}
	family := "ipv4"
	switch targetType {
	case model.TargetCIDR:
		if strings.TrimSpace(req.TargetValue) == "" {
			// 留空表示「任意来源」在界面上很好理解，但它与「忘了填」长得
			// 一模一样。要求显式写出 0.0.0.0/0 能让两者分开。
			return nil, api.InvalidParameter(
				"目标类型为 CIDR 时必须填写网段；要放行任意来源请显式写 0.0.0.0/0")
		}
	case model.TargetSwitch:
		if req.TargetValue == "" {
			return nil, api.InvalidParameter("目标类型为交换机时必须指定交换机")
		}
		if err := s.ensureSwitch(ctx, req.TargetValue, group.NodeID); err != nil {
			return nil, err
		}
	case model.TargetGroup:
		if req.TargetValue == "" {
			return nil, api.InvalidParameter("目标类型为安全组时必须指定安全组")
		}
		if err := s.ensureGroupRef(ctx, req.TargetValue, group.NodeID); err != nil {
			return nil, err
		}
	default:
		return nil, api.InvalidParameter("不支持的目标类型：" + targetType)
	}

	priority := req.Priority
	if priority <= 0 {
		priority = 100
	}

	rule := &model.SecurityGroupRule{
		GroupID: group.ID, Direction: direction, Protocol: protocol,
		PortStart: portStart, PortEnd: portEnd,
		TargetType: targetType, AddressFamily: family, Priority: priority,
	}
	if req.TargetValue != "" {
		v := req.TargetValue
		rule.TargetValue = &v
	}
	if req.Remark != "" {
		rule.Remark = &req.Remark
	}
	return rule, nil
}

// merge 把多个组的规则合并去重，并记下每条规则来自哪些组。
func merge(rules []model.SecurityGroupRule, names map[int64]string) []EffectiveRule {
	type entry struct {
		rule    EffectiveRule
		pending []string
	}
	byKey := map[string]*entry{}
	order := make([]string, 0, len(rules))

	for i := range rules {
		r := &rules[i]
		key := ruleKey(r)
		e, ok := byKey[key]
		if !ok {
			e = &entry{rule: EffectiveRule{
				Direction: r.Direction, Protocol: r.Protocol,
				PortStart: r.PortStart, PortEnd: r.PortEnd,
				TargetType: r.TargetType, AddressFamily: r.AddressFamily,
				Sources: []string{},
			}}
			if r.TargetValue != nil {
				e.rule.TargetValue = *r.TargetValue
			}
			byKey[key] = e
			order = append(order, key)
		}
		if name, ok := names[r.GroupID]; ok {
			if !containsStr(e.pending, name) {
				e.pending = append(e.pending, name)
			}
		}
	}

	out := make([]EffectiveRule, 0, len(order))
	for _, key := range order {
		e := byKey[key]
		// 来源按名字排序，让同一份数据每次渲染的顺序一致——顺序会变的
		// 列表会让人怀疑自己看错了。
		sort.Strings(e.pending)
		e.rule.Sources = e.pending
		out = append(out, e.rule)
	}
	return out
}

// ruleKey 是一条规则的**语义指纹**，用于去重。
//
// 不含 group_id 与 priority：同一条「允许入站 22 端口」写在两个组里，
// 生效结果就是一条，列两遍只会让用户以为放行了两次。
func ruleKey(r *model.SecurityGroupRule) string {
	var b strings.Builder
	b.WriteString(r.Direction)
	b.WriteByte('|')
	b.WriteString(r.Protocol)
	b.WriteByte('|')
	b.WriteString(portKey(r.PortStart))
	b.WriteByte('|')
	b.WriteString(portKey(r.PortEnd))
	b.WriteByte('|')
	b.WriteString(r.TargetType)
	b.WriteByte('|')
	if r.TargetValue != nil {
		b.WriteString(*r.TargetValue)
	}
	b.WriteByte('|')
	b.WriteString(r.AddressFamily)
	return b.String()
}

func portKey(p *int) string {
	if p == nil {
		return "*"
	}
	return strconv.Itoa(*p)
}

// versionOf 计算生效规则的指纹。
//
// 覆盖参与组的 id 与 updated_at、规则的 id 与 updated_at，以及条数：
// 增、删、改任何一处，指纹都会变。**不覆盖规则的字段值本身**——改了规则
// 就会更新 updated_at，两者等价，而只取时间戳能让计算保持廉价。
func versionOf(groups []model.SecurityGroup, rules []model.SecurityGroupRule) string {
	h := fnv.New64a()
	for i := range groups {
		fmt.Fprintf(h, "%d:%d;", groups[i].ID, groups[i].UpdatedAt.UnixNano())
	}
	h.Write([]byte("|rules|"))
	for i := range rules {
		fmt.Fprintf(h, "%d:%d;", rules[i].ID, rules[i].UpdatedAt.UnixNano())
	}
	fmt.Fprintf(h, "|n=%d", len(rules))
	return "v" + strconv.FormatUint(h.Sum64(), 16)
}

// referencedGroups 找出规则里以「安全组」为目标、但不在挂载列表里的组名。
func referencedGroups(rules []model.SecurityGroupRule, names map[int64]string) []string {
	var out []string
	for i := range rules {
		if rules[i].TargetType != model.TargetGroup || rules[i].TargetValue == nil {
			continue
		}
		id, err := strconv.ParseInt(*rules[i].TargetValue, 10, 64)
		if err != nil {
			continue
		}
		// 已在挂载列表里的不必提示：用户已经知道它生效了。
		if _, mounted := names[id]; mounted {
			continue
		}
		out = append(out, "#"+*rules[i].TargetValue)
	}
	return out
}

func (s *Service) countBy(
	ctx context.Context, model_ any, column string, ids []int64,
) (map[int64]int, error) {
	var rows []struct {
		Key int64
		N   int
	}
	if err := s.db.WithContext(ctx).Model(model_).
		Select(column+" AS key, count(*) AS n").
		Where(column+" IN ?", ids).
		Group(column).Scan(&rows).Error; err != nil {
		log.Printf("[securitygroup] 统计 %s 失败: %v", column, err)
		return nil, api.Internal()
	}
	out := make(map[int64]int, len(rows))
	for _, r := range rows {
		out[r.Key] = r.N
	}
	return out, nil
}

func (s *Service) attachedCount(ctx context.Context, groupID int64) (int, error) {
	counts, err := s.countBy(ctx, &model.InterfaceSecurityGroup{}, "group_id", []int64{groupID})
	if err != nil {
		return 0, err
	}
	var primary int64
	if err := s.db.WithContext(ctx).Model(&model.VMInterface{}).
		Where("security_group_id = ?", groupID).Count(&primary).Error; err != nil {
		log.Printf("[securitygroup] 统计主组挂载数失败: %v", err)
		return 0, api.Internal()
	}
	return counts[groupID] + int(primary), nil
}

func (s *Service) loadGroup(ctx context.Context, id int64, v authz.Viewer) (*model.SecurityGroup, error) {
	var group model.SecurityGroup
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&group).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("安全组不存在")
		}
		log.Printf("[securitygroup] 查询安全组失败: %v", err)
		return nil, api.Internal()
	}
	// 用 404 而非 403：403 会确认「这个 ID 存在」，让租户能通过枚举推断出
	// 别人有多少安全组。预置组（owner_id 为空）对所有租户可见。
	if !v.IsAdmin && group.OwnerID != nil && *group.OwnerID != v.UserID {
		return nil, api.NotFound("安全组不存在")
	}
	return &group, nil
}

func (s *Service) loadVM(ctx context.Context, id int64, v authz.Viewer) (*model.VM, error) {
	var vm model.VM
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&vm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("虚拟机不存在")
		}
		log.Printf("[securitygroup] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return nil, api.NotFound("虚拟机不存在")
	}
	return &vm, nil
}

func (s *Service) loadInterface(ctx context.Context, id int64, v authz.Viewer) (*model.VMInterface, error) {
	var iface model.VMInterface
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&iface).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("网口不存在")
		}
		log.Printf("[securitygroup] 查询网口失败: %v", err)
		return nil, api.Internal()
	}
	if _, err := s.loadVM(ctx, iface.VMID, v); err != nil {
		return nil, err
	}
	return &iface, nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	if err := s.db.WithContext(ctx).
		Select("id", "enroll_state").Where("id = ?", nodeID).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		log.Printf("[securitygroup] 查询节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("节点尚未接入")
	}
	return nil
}

func (s *Service) ensureSwitch(ctx context.Context, value string, nodeID int64) error {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return api.InvalidParameter("交换机标识不合法")
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.VpcSwitch{}).
		Where("id = ? AND node_id = ?", id, nodeID).Count(&n).Error; err != nil {
		log.Printf("[securitygroup] 查询交换机失败: %v", err)
		return api.Internal()
	}
	if n == 0 {
		// 跨节点的交换机没有网络路径，规则会静默失效——那比报错更糟。
		return api.ValidationFailed("交换机不存在或不在同一节点上")
	}
	return nil
}

func (s *Service) ensureGroupRef(ctx context.Context, value string, nodeID int64) error {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return api.InvalidParameter("安全组标识不合法")
	}
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.SecurityGroup{}).
		Where("id = ? AND node_id = ?", id, nodeID).Count(&n).Error; err != nil {
		log.Printf("[securitygroup] 查询被引用的安全组失败: %v", err)
		return api.Internal()
	}
	if n == 0 {
		return api.ValidationFailed("被引用的安全组不存在或不在同一节点上")
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func toGroupView(g *model.SecurityGroup, rules, attached int) GroupView {
	view := GroupView{
		ID: g.ID, NodeID: g.NodeID, OwnerID: g.OwnerID,
		Name: g.Name, IsDefault: g.IsDefault,
		RuleCount: rules, AttachedCount: attached,
		CreatedAt: g.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if g.Remark != nil {
		view.Remark = *g.Remark
	}
	return view
}

func toRuleView(r *model.SecurityGroupRule) RuleView {
	view := RuleView{
		ID: r.ID, GroupID: r.GroupID, Direction: r.Direction, Protocol: r.Protocol,
		PortStart: r.PortStart, PortEnd: r.PortEnd,
		TargetType: r.TargetType, AddressFamily: r.AddressFamily,
		Priority:  r.Priority,
		CreatedAt: r.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if r.TargetValue != nil {
		view.TargetValue = *r.TargetValue
	}
	if r.Remark != nil {
		view.Remark = *r.Remark
	}
	return view
}

func containsStr(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
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
