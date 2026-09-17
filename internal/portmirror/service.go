// Package portmirror 实现端口镜像与自动回滚看门狗（F-4-09）。
//
// 有一条结构性的判断贯穿本包：
//
//	**看门狗必须由节点执行，控制面只记录它的存在。**
//
// 端口镜像的失败模式是「打垮宿主机网络」，而控制面是通过网络下发指令的
// ——网络断了它就什么都做不了。一个依赖网络的保险，在它最需要起作用的
// 时候一定不在。因此：
//
//   - 启用时把「多久之后自动撤销」作为参数**一次**下发给节点；
//   - 节点自己计时，到期未收到确认就撤销；
//   - 控制面这边的 watchdog_until 是**记录**，不是执行者。
//
// 这也决定了「确认保持」是一个**可选**动作：不做它，镜像会被自动撤销。
// 安全性靠的是那个默认行为，而不是用户的记性。
package portmirror

import (
	"context"
	"errors"
	"log"
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

// 看门狗时长的边界。
//
// 下限 60 秒：太短的窗口会让用户来不及判断「网络是否正常」——而判断本身
// 就是这 个功能存在的意义。上限 30 分钟：再长的话，一个配错的镜像有足够
// 时间把生产流量灌爆，而自动撤销本来就是用来兜住这种事的。
const (
	minWatchdogSeconds     = 60
	maxWatchdogSeconds     = 1800
	defaultWatchdogSeconds = 300
)

// Service 提供端口镜像能力。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	audit *audit.Recorder
	now   func() time.Time
}

// NewService 构造服务。
func NewService(db *gorm.DB, client agent.Client, recorder *audit.Recorder) *Service {
	return &Service{db: db, agent: client, audit: recorder, now: time.Now}
}

// View 是镜像的对外视图。
type View struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"node_id"`
	Name   string `json:"name,omitempty"`

	SourcePorts    []string `json:"source_ports"`
	TargetSwitches []string `json:"target_switches"`
	Direction      string   `json:"direction"`
	VlanPreserve   bool     `json:"vlan_preserve"`

	Enabled bool `json:"enabled"`
	// AwaitingConfirm 为 true 表示正处于**看门狗窗口**里。
	//
	// 界面必须显眼地标出它：窗口一旦过去而用户没确认，镜像会被节点自动
	// 撤销，而那时他会以为是系统出了问题。
	AwaitingConfirm bool   `json:"awaiting_confirm"`
	WatchdogUntil   string `json:"watchdog_until,omitempty"`
	// WatchdogSecondsLeft 由**服务端**算好下发，而不是让界面拿时刻去减
	// 本地时间：客户端时钟不准是常态，而「还剩 3 分 12 秒」必须可信。
	WatchdogSecondsLeft int    `json:"watchdog_seconds_left"`
	LastAppliedAt       string `json:"last_applied_at,omitempty"`
	CreatedAt           string `json:"created_at"`
}

// Request 是创建或更新一条镜像规则的请求。
type Request struct {
	NodeID         int64
	Name           string
	SourcePorts    []string
	TargetSwitches []string
	Direction      string
	VlanPreserve   bool
}

// List 返回节点上的镜像规则。
func (s *Service) List(ctx context.Context, nodeID int64) ([]View, error) {
	var rows []model.PortMirror
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).Order("id ASC").Find(&rows).Error; err != nil {
		log.Printf("[portmirror] 查询失败: %v", err)
		return nil, api.Internal()
	}
	views := make([]View, 0, len(rows))
	for i := range rows {
		views = append(views, s.toView(&rows[i]))
	}
	return views, nil
}

// Create 新建一条镜像规则（默认**不启用**）。
//
// 创建与启用分开是刻意的：创建只写控制面，启用才会动宿主机的网络——而
// 后者带着看门狗与自动回滚，是一个需要用户明确表达意图的动作。
func (s *Service) Create(
	ctx context.Context, req Request, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	n, err := normalize(req)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}

	row := model.PortMirror{
		NodeID: req.NodeID, Direction: n.direction,
		VlanPreserve: req.VlanPreserve, Enabled: false,
	}
	name, sources, targets := n.name, strings.Join(n.sources, "\n"), strings.Join(n.targets, "\n")
	row.Name, row.SourcePorts, row.TargetSwitches = &name, &sources, &targets

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[portmirror] 创建失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "port_mirror", ResourceID: row.ID,
		ResourceName: n.name, Action: "port_mirror.create",
		Params: n.auditParams(), Success: true, ClientIP: clientIP,
	})
	view := s.toView(&row)
	return &view, nil
}

// Update 修改一条**未启用**的规则。
//
// 已启用时拒绝修改：改一条正在生效的镜像等于在一次操作里同时做「撤掉旧的」
// 和「建起新的」两件事，而后者带着一个自己没被看门狗保护的窗口。要改就先
// 关掉——那一步是明确的，用户知道自己此刻把什么停掉了。
func (s *Service) Update(
	ctx context.Context, id int64, req Request, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.Enabled {
		return nil, api.ValidationFailed(
			"该镜像正在生效，请先关闭再修改——直接改会在一次操作里同时撤掉旧的、" +
				"建起新的，而新的那一段没有被看门狗保护")
	}
	n, err := normalize(req)
	if err != nil {
		return nil, err
	}

	name, sources, targets := n.name, strings.Join(n.sources, "\n"), strings.Join(n.targets, "\n")
	row.Name, row.SourcePorts, row.TargetSwitches = &name, &sources, &targets
	row.Direction, row.VlanPreserve = n.direction, req.VlanPreserve
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		log.Printf("[portmirror] 更新失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "port_mirror", ResourceID: row.ID,
		ResourceName: n.name, Action: "port_mirror.update",
		Params: n.auditParams(), Success: true, ClientIP: clientIP,
	})
	view := s.toView(row)
	return &view, nil
}

// Delete 删除一条**未启用**的规则。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	row, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if row.Enabled {
		return api.ValidationFailed("该镜像正在生效，请先关闭再删除")
	}
	if err := s.db.WithContext(ctx).Delete(&model.PortMirror{}, row.ID).Error; err != nil {
		log.Printf("[portmirror] 删除失败: %v", err)
		return api.Internal()
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "port_mirror", ResourceID: row.ID,
		Action: "port_mirror.delete", Success: true, ClientIP: clientIP,
	})
	return nil
}

// Precheck 返回启用前的静态检查结论。
//
// **这只是一层静态检查**：它能看到的是控制面记录里的重叠关系，而真正的
// 环路取决于宿主机上的实际拓扑（哪个接口挂在哪个桥上、哪些口是 UP 的），
// 那只有节点知道。因此预检通过**不代表一定安全**——它拦掉的是最容易犯的
// 那几类错误，兜住剩下那些的是看门狗。
//
// 这个分工值得写下来：如果预检被当成「安全保证」，看门狗就会被当成多余的。
func (s *Service) Precheck(ctx context.Context, id int64) ([]string, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	sources, targets := splitList(row.SourcePorts), splitList(row.TargetSwitches)
	if len(sources) == 0 {
		return nil, api.ValidationFailed("该镜像没有来源接口")
	}
	if len(targets) == 0 {
		return nil, api.ValidationFailed(
			"该镜像没有目标交换机——镜像出去的流量必须有地方接，" +
				"否则它会堆在宿主机的发送队列里")
	}

	var others []model.PortMirror
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND id <> ? AND enabled = ?", row.NodeID, row.ID, true).
		Find(&others).Error; err != nil {
		log.Printf("[portmirror] 查询其它规则失败: %v", err)
		return nil, api.Internal()
	}
	enabledSources, enabledTargets := map[string]string{}, map[string]string{}
	for i := range others {
		label := mirrorLabel(&others[i])
		for _, p := range splitList(others[i].SourcePorts) {
			enabledSources[p] = label
		}
		for _, t := range splitList(others[i].TargetSwitches) {
			enabledTargets[t] = label
		}
	}

	var warnings []string
	for _, p := range sources {
		if label, dup := enabledSources[p]; dup {
			warnings = append(warnings,
				"来源接口 "+p+" 已被另一条生效中的镜像「"+label+"」使用："+
					"同一个口的流量会被复制两份")
		}
	}
	for _, t := range targets {
		if label, dup := enabledTargets[t]; dup {
			warnings = append(warnings,
				"目标交换机 "+t+" 已被另一条生效中的镜像「"+label+"」使用："+
					"两条镜像的流量会灌进同一个地方")
		}
	}
	if row.Direction == model.MirrorBoth {
		warnings = append(warnings,
			"方向为「双向」：任一方向上的环路都会被两个方向同时放大。"+
				"只分析一个方向时选单向更安全")
	}

	// 这一条不是风险，是**背景说明**：预检能力的边界。它由 filterRisky
	// 排除在「需要确认」之外——否则它会稀释真正需要看的那些警告。
	warnings = append(warnings,
		"预检只看控制面记录里的重叠关系。真正的环路取决于宿主机上的实际拓扑"+
			"（接口挂在哪、哪些口是 UP），那只有节点知道——"+
			"因此启用后会有一个自动撤销窗口，到点未确认即自动关闭")

	return warnings, nil
}

// EnableRequest 是启用请求。
type EnableRequest struct {
	// WatchdogSeconds 是自动撤销窗口；为 0 时取默认值。
	//
	// **不能设为「无」**：一个没有兜底的端口镜像变更，配错时唯一的恢复
	// 途径是上宿主机敲命令——而那正是这个功能要避免的事情。
	WatchdogSeconds int
	Acknowledge     bool
}

// EnableResult 是启用结果。
type EnableResult struct {
	Applied         bool     `json:"applied"`
	WatchdogSeconds int      `json:"watchdog_seconds"`
	Warnings        []string `json:"warnings,omitempty"`
	Mirror          *View    `json:"mirror,omitempty"`
}

// Enable 启用镜像，并**在同一次下发里**建立看门狗。
//
// 顺序是这条规则的全部意义：看门狗与镜像必须一起生效，中间不能有窗口
// ——见 agent.OpPortMirrorEnable 的说明。
//
// 有风险警告而未确认时**不下发、也不报错**，而是把警告返回。与防火墙的
// 预检同一套语义：那是一个需要用户做决定的岔路口，不是一次失败。
func (s *Service) Enable(
	ctx context.Context, id int64, req EnableRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*EnableResult, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.Enabled {
		return nil, api.ValidationFailed("该镜像已经启用")
	}

	seconds := req.WatchdogSeconds
	if seconds <= 0 {
		seconds = defaultWatchdogSeconds
	}
	if seconds < minWatchdogSeconds || seconds > maxWatchdogSeconds {
		return nil, api.InvalidParameter(
			"自动撤销窗口必须在 " + strconv.Itoa(minWatchdogSeconds) + " 秒到 " +
				strconv.Itoa(maxWatchdogSeconds) + " 秒之间")
	}

	warnings, err := s.Precheck(ctx, id)
	if err != nil {
		return nil, err
	}
	risky := filterRisky(warnings)
	if !req.Acknowledge && len(risky) > 0 {
		return &EnableResult{Warnings: warnings, Applied: false}, nil
	}

	// 看门狗时刻在控制面先算好，随指令一起下发（节点按秒数计时）。
	// 让节点自己算「从现在起 N 秒」也行，但那样两边对这个时刻的理解会差
	// 一个网络往返；控制面先算则两边只差那一次往返，远小于窗口本身。
	until := s.now().Add(time.Duration(seconds) * time.Second)

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPortMirrorEnable,
		NodeID: row.NodeID,
		Target: mirrorLabel(row),
		Params: map[string]any{
			"name":             mirrorLabel(row),
			"source_ports":     splitList(row.SourcePorts),
			"target_switches":  splitList(row.TargetSwitches),
			"direction":        row.Direction,
			"vlan_preserve":    row.VlanPreserve,
			"watchdog_seconds": seconds,
		},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，镜像未启用")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	now := s.now()
	if err := s.db.WithContext(ctx).Model(&model.PortMirror{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"enabled": true, "watchdog_until": until, "last_applied_at": now,
		}).Error; err != nil {
		log.Printf("[portmirror] 回写启用状态失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "port_mirror", ResourceID: row.ID,
		ResourceName: mirrorLabel(row), Action: "port_mirror.enable",
		Params: map[string]any{
			"watchdog_seconds":      seconds,
			"watchdog_until":        until.Format(time.RFC3339),
			"acknowledged_warnings": risky,
		},
		Success: true, ClientIP: clientIP,
	})

	// 把内存里的行同步到刚写入的状态再构造视图。
	//
	// row 是在**更新之前**读出来的，它的 Enabled 还是 false；不刷新就会
	// 得到一个「已启用但显示为未启用、也没有确认窗口」的响应——而客户端
	// 完全有理由相信这个响应，它刚看着服务端返回了成功。
	row.Enabled = true
	view := s.toView2(row, &until, &now)
	return &EnableResult{
		Applied: true, WatchdogSeconds: seconds, Warnings: warnings, Mirror: &view,
	}, nil
}

// Confirm 确认「保持」，取消看门狗。
//
// 这是一个**可选**动作：不做它，镜像会被自动撤销。安全性靠的是那个默认
// 行为，而不是用户的记性——这也是为什么它不需要二次验证：确认保持只是让
// 一个已经生效的东西继续生效，不产生新的风险。
func (s *Service) Confirm(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !row.IsAwaitingConfirm() {
		return nil, api.ValidationFailed("该镜像当前没有待确认的自动撤销窗口")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpPortMirrorConfirm, NodeID: row.NodeID, Target: mirrorLabel(row),
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法取消自动撤销")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Model(&model.PortMirror{}).
		Where("id = ?", row.ID).Update("watchdog_until", nil).Error; err != nil {
		log.Printf("[portmirror] 取消看门狗失败: %v", err)
		return nil, api.Internal()
	}
	row.WatchdogUntil = nil

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "port_mirror", ResourceID: row.ID,
		ResourceName: mirrorLabel(row), Action: "port_mirror.confirm",
		Success: true, ClientIP: clientIP,
	})
	view := s.toView(row)
	return &view, nil
}

// Disable 关闭镜像。
//
// 与防火墙的紧急回滚同一套思路：**不做确认、不做预检**。一个正在生效的
// 端口镜像如果已经在打垮网络，用户需要的是一键关掉。
func (s *Service) Disable(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !row.Enabled {
		// 幂等：重复关闭不是错误。
		view := s.toView(row)
		return &view, nil
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpPortMirrorDisable, NodeID: row.NodeID, Target: mirrorLabel(row),
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法关闭镜像")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	if err := s.db.WithContext(ctx).Model(&model.PortMirror{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{"enabled": false, "watchdog_until": nil}).Error; err != nil {
		log.Printf("[portmirror] 关闭失败: %v", err)
		return nil, api.Internal()
	}
	row.Enabled, row.WatchdogUntil = false, nil

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "port_mirror", ResourceID: row.ID,
		ResourceName: mirrorLabel(row), Action: "port_mirror.disable",
		Success: true, ClientIP: clientIP,
	})
	view := s.toView(row)
	return &view, nil
}

// Reconcile 把「看门狗已到期」的镜像在控制面这边标记为已关闭。
//
// 它**不是**回滚的执行者——执行者是节点（它到点会自己撤销）。这里做的是
// 让控制面的记录追上节点已经做完的事。
//
// 两者分开是必须的：如果只有控制面这一侧扫，那么在一次面板停机或网络中断
// 期间启用的镜像就永远不会被撤销。节点的看门狗保证**撤销一定发生**，
// 而这里的对齐保证**用户能看到它发生了**。
func (s *Service) Reconcile(ctx context.Context) (int, error) {
	res := s.db.WithContext(ctx).Model(&model.PortMirror{}).
		Where("enabled = ? AND watchdog_until IS NOT NULL AND watchdog_until < ?",
			true, s.now()).
		Updates(map[string]any{"enabled": false, "watchdog_until": nil})
	if res.Error != nil {
		log.Printf("[portmirror] 对齐到期看门狗失败: %v", res.Error)
		return 0, api.Internal()
	}
	if res.RowsAffected > 0 {
		log.Printf("[portmirror] %d 条镜像的看门狗已到期，节点应已自动撤销", res.RowsAffected)
	}
	return int(res.RowsAffected), nil
}

// --- 内部 ---

type normalized struct {
	name      string
	sources   []string
	targets   []string
	direction string
}

func (n normalized) auditParams() map[string]any {
	return map[string]any{
		"name": n.name, "source_ports": n.sources,
		"target_switches": n.targets, "direction": n.direction,
	}
}

func normalize(req Request) (normalized, error) {
	sources := cleanList(req.SourcePorts)
	if len(sources) == 0 {
		return normalized{}, api.InvalidParameter("至少需要一个来源接口")
	}
	targets := cleanList(req.TargetSwitches)
	if len(targets) == 0 {
		return normalized{}, api.InvalidParameter(
			"至少需要一个目标交换机——镜像出去的流量必须有地方接")
	}

	// 来源不能重复：同一个口的流量被复制两份是**静默的流量放大**，而它在
	// 抓包里表现为「每个包都出现两次」，很容易被误判成对端在重传。
	if dup := firstDuplicate(sources); dup != "" {
		return normalized{}, api.InvalidParameter("来源接口重复：" + dup)
	}
	if dup := firstDuplicate(targets); dup != "" {
		return normalized{}, api.InvalidParameter("目标交换机重复：" + dup)
	}
	for _, p := range sources {
		for _, t := range targets {
			if p == t {
				return normalized{}, api.InvalidParameter(
					"同一个对象不能既是来源又是目标：" + p + "——那会把它的流量复制回自己")
			}
		}
	}

	direction := req.Direction
	if direction == "" {
		direction = model.MirrorBoth
	}
	if !model.ValidMirrorDirection(direction) {
		return normalized{}, api.InvalidParameter("方向必须是 both / ingress / egress")
	}

	name := strings.TrimSpace(req.Name)
	if len(name) > 64 {
		return normalized{}, api.InvalidParameter("名称最长 64 字")
	}
	if name == "" {
		// 没名字时用第一个来源口做标识：一个完全没有标识的规则在界面上只能
		// 靠 id 分辨，而 id 是给机器看的。
		name = "镜像-" + sources[0]
	}
	return normalized{name: name, sources: sources, targets: targets, direction: direction}, nil
}

func (s *Service) load(ctx context.Context, id int64) (*model.PortMirror, error) {
	var row model.PortMirror
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("镜像规则不存在")
		}
		log.Printf("[portmirror] 查询失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	if err := s.db.WithContext(ctx).
		Select("id", "enroll_state").Where("id = ?", nodeID).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		log.Printf("[portmirror] 查询节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("节点尚未接入")
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func (s *Service) toView(row *model.PortMirror) View {
	return s.toView2(row, row.WatchdogUntil, row.LastAppliedAt)
}

func (s *Service) toView2(row *model.PortMirror, until, applied *time.Time) View {
	view := View{
		ID: row.ID, NodeID: row.NodeID,
		SourcePorts:    splitList(row.SourcePorts),
		TargetSwitches: splitList(row.TargetSwitches),
		Direction:      row.Direction, VlanPreserve: row.VlanPreserve,
		Enabled:         row.Enabled,
		AwaitingConfirm: row.Enabled && until != nil,
		CreatedAt:       row.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if row.Name != nil {
		view.Name = *row.Name
	}
	if until != nil {
		view.WatchdogUntil = until.Format("2006-01-02T15:04:05Z07:00")
		left := int(until.Sub(s.now()).Seconds())
		if left < 0 {
			left = 0
		}
		view.WatchdogSecondsLeft = left
	}
	if applied != nil {
		view.LastAppliedAt = applied.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

func mirrorLabel(m *model.PortMirror) string {
	if m.Name != nil && *m.Name != "" {
		return *m.Name
	}
	return "镜像#" + strconv.FormatInt(m.ID, 10)
}

// filterRisky 从预检结论里挑出**需要用户确认**的那几条。
//
// 预检的输出混了两类东西：真正的风险（重叠、双向），以及背景说明（预检
// 能力的边界）。后者不该拦住一次启用——把它也算成「需要确认」会让用户
// 习惯性地点过确认框，而那时真正的风险提示也就失去了作用。
func filterRisky(warnings []string) []string {
	out := make([]string, 0, len(warnings))
	for _, w := range warnings {
		if strings.HasPrefix(w, "预检只看控制面记录") {
			continue
		}
		out = append(out, w)
	}
	return out
}

func cleanList(list []string) []string {
	out := make([]string, 0, len(list))
	for _, raw := range list {
		if v := strings.TrimSpace(raw); v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func firstDuplicate(list []string) string {
	seen := map[string]bool{}
	for _, v := range list {
		if seen[v] {
			return v
		}
		seen[v] = true
	}
	return ""
}

func splitList(raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return []string{}
	}
	fields := strings.FieldsFunc(*raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}
