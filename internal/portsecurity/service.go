// Package portsecurity 实现端口安全（F-4-08）。
//
// 三件事：源地址防伪造、端口隔离、包速率限制。三者的**性质不同**，而这块
// 功能最容易出错的地方正是不区分它们：
//
//	防伪造   安全边界，且是**其它一切的前提**（见 model.PortSecurityPolicy）
//	隔离     安全边界，但代价大：同网段内全都不通，包括用户自己的集群
//	限速     资源保护，不是安全措施
//
// 因此默认值也不同：安全性相关的默认关（开了会断网，必须由用户明确决定），
// 限速默认不限（默认限流会在用户什么都没做的时候开始丢包）。
//
// 另一条贯穿本包的原则：**能力不具备时拒绝启用，而不是接受一个不会生效的
// 配置**。端口安全依赖 Open vSwitch，而节点上很可能没有装。悄悄收下一个
// 配置、把它标成"已启用"，用户会以为攻击面已经被收住了——那比明说"不支持"
// 危险得多。
package portsecurity

import (
	"context"
	"errors"
	"fmt"
	"log"
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

// 包速率限制的边界。
//
// 下限 10 pps：比这更低的限制会让 SSH 这类交互式会话都卡到不可用，用户
// 多半是把单位想错了（kpps 填成 pps）。上限 100 万 pps：单口跑到这个量级
// 已经接近线速，再往上填没有意义，而一个填错的大数字会让人误以为限住了。
const (
	minPPSLimit = 10
	maxPPSLimit = 1_000_000
)

// Service 提供端口安全能力。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	audit *audit.Recorder
	// queue 用于下发：策略要写进节点上的流表，因此必须走任务队列。
	queue *task.Queue
	now   func() time.Time
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, client agent.Client, recorder *audit.Recorder, queue *task.Queue,
) *Service {
	return &Service{db: db, agent: client, audit: recorder, queue: queue, now: time.Now}
}

// View 是端口安全策略的对外视图。
type View struct {
	ID       int64  `json:"id"`
	NodeID   int64  `json:"node_id"`
	PortRef  string `json:"port_ref"`
	SwitchID int64  `json:"switch_id,omitempty"`
	VMID     int64  `json:"vm_id,omitempty"`
	// VMName 让用户能对上是哪台机器——只给 ID 的话还得再去查一次列表。
	VMName string `json:"vm_name,omitempty"`

	SpoofingGuard bool `json:"spoofing_guard"`
	Isolation     bool `json:"isolation"`
	PPSLimit      int  `json:"pps_limit"`

	Status string `json:"status"`
	// AppliedAt 为空表示尚未生效。与「已启用」是两回事，界面要分开说。
	AppliedAt string `json:"applied_at,omitempty"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at"`
}

// Request 是配置端口安全的请求。
type Request struct {
	NodeID   int64
	PortRef  string
	SwitchID int64
	VMID     int64

	SpoofingGuard bool
	Isolation     bool
	PPSLimit      int
}

// Precheck 是预检结果。
//
// 它是这块功能的核心入口而不是一个附加步骤：把配置下到一个网口上的后果
// （同网段失联）是不可从界面直接看出来的，而**能力是否具备**更不能等到
// 点下"启用"才告诉用户。
type Precheck struct {
	// CanApply 为 false 时**不能启用**，Reasons 说明缺什么。
	CanApply bool `json:"can_apply"`
	// Capabilities 是三项能力各自的可用状态。
	Capabilities []CapabilityState `json:"capabilities"`
	// Rules 是将要下发的流表（人可读）。
	//
	// 必须来自节点：最终写下去的规则取决于该网口当前所属的网桥、已有的
	// 流表与端口号，控制面按模板拼一段出来看起来对、实际可能差很远。
	Rules []string `json:"rules"`
	// Warnings 是需要用户先看一眼的后果。
	Warnings []string `json:"warnings"`
}

// CapabilityState 是一项能力的可用状态。
type CapabilityState struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	// Missing 为 true 表示节点上不具备这项能力。
	Missing bool `json:"missing"`
	// Reason / Fix 说明缺什么、怎么装。给出命令而不是只说"不支持"：
	// 用户看到"不支持"只能放弃，看到安装命令则能在几分钟内解决。
	Reason string `json:"reason,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// List 返回节点上的端口安全策略。
func (s *Service) List(ctx context.Context, nodeID int64) ([]View, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	var rows []model.PortSecurityPolicy
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("port_ref ASC").Find(&rows).Error; err != nil {
		log.Printf("[portsecurity] 查询失败: %v", err)
		return nil, api.Internal()
	}
	names := s.vmNames(ctx, rows)

	out := make([]View, 0, len(rows))
	for i := range rows {
		v := toView(&rows[i])
		if rows[i].VMID != nil {
			v.VMName = names[*rows[i].VMID]
		}
		out = append(out, v)
	}
	return out, nil
}

// Preview 预检：探测能力并给出将要下发的规则。**只读**。
func (s *Service) Preview(ctx context.Context, req Request) (*Precheck, error) {
	if err := s.validate(req); err != nil {
		return nil, err
	}
	// 节点必须存在。**不能只靠 agent 报错**：agent 对不存在的节点也会
	// 照常返回（它不知道控制面的节点表），于是预检会对一台并不存在的机器
	// 给出"可以启用"，而用户点了之后才知道有问题。
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}
	return s.precheck(ctx, req)
}

// Apply 配置并启用端口安全策略。
//
// `acknowledge` 为 false 且有警告时**不执行、也不报错**，而是把预检结果
// 原样返回——与防火墙、端口镜像、存储卷同一套语义：那是一个需要用户做
// 决定的岔路口，不是一次失败。
func (s *Service) Apply(
	ctx context.Context, req Request, acknowledge bool,
	v authz.Viewer, operatorName, clientIP string,
) (*Precheck, *model.PortSecurityPolicy, *string, error) {
	check, err := s.Preview(ctx, req)
	if err != nil {
		return nil, nil, nil, err
	}
	if !check.CanApply {
		// 能力不具备时**直接拒绝**，而不是收下配置标成 pending。
		// 一份不会生效的"已启用"策略，用户会以为攻击面已经收住了。
		return check, nil, nil, api.ValidationFailed(strings.Join(reasonsOf(check), "；"))
	}
	if len(check.Warnings) > 0 && !acknowledge {
		return check, nil, nil, nil
	}

	row, err := s.upsert(ctx, req)
	if err != nil {
		return check, nil, nil, err
	}

	t, err := s.queueTask(ctx, *row, v)
	if err != nil {
		return check, nil, nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "port_security",
		ResourceID: row.ID, ResourceName: req.PortRef,
		Action: "port_security.apply",
		Params: map[string]any{
			"port_ref":              req.PortRef,
			"spoofing_guard":        req.SpoofingGuard,
			"isolation":             req.Isolation,
			"pps_limit":             req.PPSLimit,
			"acknowledged_warnings": check.Warnings,
		},
		Success: true, ClientIP: clientIP,
	})
	return check, row, t, nil
}

// Disable 停用并移除端口安全策略。
//
// 不需要二次验证：策略关掉之后同网段立刻恢复互通，而这种变化是**立刻可见**
// 的——用户马上就知道发生了什么。
func (s *Service) Disable(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*string, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !row.Enabled() {
		// 已经什么都没开的策略再"停用"一次是无意义的操作，而静默成功会让
		// 用户以为它刚才确实生效着。
		return nil, api.ValidationFailed("该策略未启用任何保护")
	}

	// **先把配置清零，再下发。**
	//
	// 顺序不能反：下发的是期望状态（见 agent.OpPortSecurityApply），而
	// 期望状态来自这条记录。不先清零的话下发的还是"开着"的那一份，节点
	// 上的规则**一条都不会被撤销**——而界面上会显示"已停用"。
	if err := s.db.WithContext(ctx).Model(&model.PortSecurityPolicy{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"spoofing_guard": false, "isolation": false, "pps_limit": 0,
			"status": model.PortSecurityPending,
		}).Error; err != nil {
		log.Printf("[portsecurity] 清零策略失败: %v", err)
		return nil, api.Internal()
	}
	if err := s.clearAppliedAt(ctx, row.ID); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).First(row, row.ID).Error; err != nil {
		return nil, api.Internal()
	}

	t, err := s.queueTask(ctx, *row, v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "port_security",
		ResourceID: row.ID, ResourceName: row.PortRef,
		Action:  "port_security.disable",
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// --- 内部 ---

func (s *Service) validate(req Request) error {
	if strings.TrimSpace(req.PortRef) == "" {
		return api.InvalidParameter("必须指定网口")
	}
	if req.PPSLimit < 0 {
		return api.InvalidParameter("包速率限制不能为负")
	}
	if req.PPSLimit > 0 {
		if req.PPSLimit < minPPSLimit {
			// 单位想错（kpps 填成 pps）是最常见的原因，直接把它说出来。
			return api.InvalidParameter(fmt.Sprintf(
				"包速率限制不能低于 %d pps——比这更低会让 SSH 这类交互式会话卡到不可用，"+
					"请确认没有把单位想错（是要填每秒包数，不是每秒千包）", minPPSLimit))
		}
		if req.PPSLimit > maxPPSLimit {
			return api.InvalidParameter(fmt.Sprintf(
				"包速率限制不能超过 %d pps——超过这个量级已经接近单口线速，填它没有意义", maxPPSLimit))
		}
	}
	return nil
}

// precheck 探测能力并生成预检结果。
func (s *Service) precheck(ctx context.Context, req Request) (*Precheck, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPortSecurityPrecheck,
		NodeID: req.NodeID,
		Target: req.PortRef,
		Params: map[string]any{
			"port_ref":       req.PortRef,
			"spoofing_guard": req.SpoofingGuard,
			"isolation":      req.Isolation,
			"pps_limit":      req.PPSLimit,
		},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法预检端口安全")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	check := &Precheck{CanApply: true, Capabilities: []CapabilityState{}, Rules: []string{}}
	if info, ok := result.Data[agent.PortSecurityPrecheckKey].(agent.PortSecurityPrecheck); ok {
		// 逐项转换而不是直接赋值：agent 包的类型是**协议类型**，而对外
		// 视图是接口类型。让它们共用一个结构会让协议的改动直接泄漏到
		// 界面上——加一个内部字段就改了 API。
		for _, c := range info.Capabilities {
			check.Capabilities = append(check.Capabilities, CapabilityState{
				Key: c.Key, Label: c.Label, Required: c.Required,
				Missing: c.Missing, Reason: c.Reason, Fix: c.Fix,
			})
		}
		check.Rules = append(check.Rules, info.Rules...)
		check.Warnings = append(check.Warnings, info.Warnings...)
	}

	// 能力门禁：缺必需能力时不能启用。
	for _, c := range check.Capabilities {
		if c.Required && c.Missing {
			check.CanApply = false
		}
	}

	// 与配置本身有关的后果提示（与节点无关，因此在这里加）。
	if req.Isolation {
		check.Warnings = append(check.Warnings,
			"端口隔离会让该网口与其所在二层网段内**所有**其它机器互不可见——"+
				"包括您自己放在同一网段、需要互通的应用（集群、主从、心跳）。"+
				"它们之间的连接会在启用后立刻断开。")
	}
	if req.SpoofingGuard {
		// 这条不是警告而是说明：防伪造是**默认应当开**的一项，把它写成
		// 警告会让人以为它有害。
		check.Rules = append(check.Rules,
			"# 防伪造：仅允许该网口使用属于它的源 IP 与源 MAC，"+
				"其余源地址的报文直接丢弃")
	}
	return check, nil
}

// upsert 写入或更新策略。
func (s *Service) upsert(ctx context.Context, req Request) (*model.PortSecurityPolicy, error) {
	var row model.PortSecurityPolicy
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND port_ref = ?", req.NodeID, req.PortRef).
		First(&row).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("[portsecurity] 查询已有策略失败: %v", err)
		return nil, api.Internal()
	}

	updates := map[string]any{
		"spoofing_guard": req.SpoofingGuard,
		"isolation":      req.Isolation,
		"pps_limit":      req.PPSLimit,
		// 配置变了就回到 pending：节点上还是旧规则，而这期间界面上显示的
		// "已启用"会让人以为新配置已经生效。
		"status": model.PortSecurityPending,
	}
	if req.SwitchID > 0 {
		updates["switch_id"] = req.SwitchID
	}
	if req.VMID > 0 {
		updates["vm_id"] = req.VMID
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = model.PortSecurityPolicy{
			NodeID: req.NodeID, PortRef: req.PortRef,
			SpoofingGuard: req.SpoofingGuard, Isolation: req.Isolation,
			PPSLimit: req.PPSLimit, Status: model.PortSecurityPending,
		}
		if req.SwitchID > 0 {
			row.SwitchID = &req.SwitchID
		}
		if req.VMID > 0 {
			row.VMID = &req.VMID
		}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			log.Printf("[portsecurity] 创建策略失败: %v", err)
			return nil, api.Internal()
		}
		return &row, nil
	}

	if err := s.db.WithContext(ctx).Model(&model.PortSecurityPolicy{}).
		Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		log.Printf("[portsecurity] 更新策略失败: %v", err)
		return nil, api.Internal()
	}
	// **applied_at 用一条单独的语句清空。**
	//
	// GORM 的 `Updates(map)` 会跳过值为 nil 的项，而这里恰恰必须写成 NULL
	// ——配置改了而 applied_at 还留着上一次的时间，界面上会显示"已生效"，
	// 而节点上跑的是旧规则。这类"看起来生效了"的偏差不报任何错。
	if err := s.clearAppliedAt(ctx, row.ID); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).First(&row, row.ID).Error; err != nil {
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) queueTask(
	ctx context.Context, row model.PortSecurityPolicy, v authz.Viewer,
) (*string, error) {
	// 停用就是把三项全部关掉再下发一次——而不是"下发一条删除指令"。
	//
	// 理由：节点上的实际状态可能已经被别人改过（手工加了流表、节点重启过），
	// 一条"删除"指令在那种情况下什么也删不掉，而**下发一份全关的期望状态**
	// 无论当前是什么样都能收敛到正确结果。
	t, err := s.enqueue(ctx, row, v)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// clearAppliedAt 把 applied_at 置空。
//
// 单独一条语句：GORM 的 `Updates(map)` 跳过 nil 值，而这里必须写成 NULL。
func (s *Service) clearAppliedAt(ctx context.Context, id int64) error {
	if err := s.db.WithContext(ctx).Model(&model.PortSecurityPolicy{}).
		Where("id = ?", id).
		Update("applied_at", gorm.Expr("NULL")).Error; err != nil {
		log.Printf("[portsecurity] 清空 applied_at 失败 id=%d: %v", id, err)
		return api.Internal()
	}
	return nil
}

func (s *Service) load(ctx context.Context, id int64) (*model.PortSecurityPolicy, error) {
	var row model.PortSecurityPolicy
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("端口安全策略不存在")
		}
		log.Printf("[portsecurity] 查询失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var node model.Node
	if err := s.db.WithContext(ctx).Select("id").First(&node, nodeID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		return api.Internal()
	}
	return nil
}

func (s *Service) vmNames(ctx context.Context, rows []model.PortSecurityPolicy) map[int64]string {
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		if rows[i].VMID != nil {
			ids = append(ids, *rows[i].VMID)
		}
	}
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var vms []model.VM
	if err := s.db.WithContext(ctx).Select("id", "name").
		Where("id IN ?", ids).Find(&vms).Error; err != nil {
		// 名字只是展示用的补充信息，查不到不该让整个列表失败。
		log.Printf("[portsecurity] 查询虚拟机名失败: %v", err)
		return out
	}
	for _, vm := range vms {
		out[vm.ID] = vm.Name
	}
	return out
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

// reasonsOf 汇总不能启用的原因。
func reasonsOf(check *Precheck) []string {
	out := []string{}
	for _, c := range check.Capabilities {
		if c.Required && c.Missing {
			msg := c.Label + "不可用"
			if c.Reason != "" {
				msg += "（" + c.Reason + "）"
			}
			if c.Fix != "" {
				msg += "，安装：" + c.Fix
			}
			out = append(out, msg)
		}
	}
	if len(out) == 0 {
		out = append(out, "当前节点不具备所需能力")
	}
	return out
}

func toView(p *model.PortSecurityPolicy) View {
	v := View{
		ID: p.ID, NodeID: p.NodeID, PortRef: p.PortRef,
		SpoofingGuard: p.SpoofingGuard, Isolation: p.Isolation, PPSLimit: p.PPSLimit,
		Status:    p.Status,
		CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if p.SwitchID != nil {
		v.SwitchID = *p.SwitchID
	}
	if p.VMID != nil {
		v.VMID = *p.VMID
	}
	if p.AppliedAt != nil {
		v.AppliedAt = p.AppliedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if p.Detail != nil {
		v.Detail = *p.Detail
	}
	return v
}

// enqueue 把策略下发入队，返回任务 ID 文本（供审计与界面引用）。
func (s *Service) enqueue(
	ctx context.Context, row model.PortSecurityPolicy, v authz.Viewer,
) (*string, error) {
	vmName := ""
	if row.VMID != nil {
		if names := s.vmNames(ctx, []model.PortSecurityPolicy{row}); names != nil {
			vmName = names[*row.VMID]
		}
	}
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskPortSecurityApply,
		NodeID:       row.NodeID,
		ResourceType: "port_security",
		ResourceID:   row.ID,
		ResourceName: row.PortRef,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"policy_id": row.ID,
			"port_ref":  row.PortRef,
			"vm_name":   vmName,
			// 期望状态，而不是增量指令（见 agent.OpPortSecurityApply）。
			"spoofing_guard": row.SpoofingGuard,
			"isolation":      row.Isolation,
			"pps_limit":      row.PPSLimit,
		},
	})
	if err != nil {
		log.Printf("[portsecurity] 任务入队失败: %v", err)
		return nil, api.Internal()
	}
	id := strconv.FormatInt(t.ID, 10)
	return &id, nil
}
