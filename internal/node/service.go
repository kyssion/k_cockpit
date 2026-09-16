// Package node 实现节点管理（F-6-01 / F-6-02）。
//
// 职责边界：控制面管理节点的**元数据**（名称、注册信息、维护模式），
// 节点的**运行态**（心跳、能力、版本）只可能来自 agent 上报，控制面
// 代为缓存用于展示与离线判定。因此本包依赖 agent.SnapshotProvider：
// 开发期由 mock 提供，接入真实节点时无需改动本包（ADR-0007）。
package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// OfflineThreshold 是离线判定阈值（f-6-01 R-005）。
//
// 心跳周期 10s，阈值取 40s：允许连续丢失 3 次心跳后仍判为在线，
// 避免网络抖动导致状态反复跳变。
const OfflineThreshold = 40 * time.Second

// DefaultEnrollTTL 是注册令牌的默认有效期（f-6-01 Q-003：24 小时）。
const DefaultEnrollTTL = 24 * time.Hour

// namePattern 限定节点名可用字符。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Service 提供节点领域操作。
type Service struct {
	db      *gorm.DB
	runtime agent.SnapshotProvider
	audit   *audit.Recorder
}

// NewService 构造节点服务。
func NewService(db *gorm.DB, runtime agent.SnapshotProvider, recorder *audit.Recorder) *Service {
	return &Service{db: db, runtime: runtime, audit: recorder}
}

// View 是节点的对外视图：元数据 + 运行态。
type View struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// Status 是**推导出的**运行态，不是数据库里的字段值（见 deriveStatus）。
	Status          string `json:"status"`
	EnrollState     string `json:"enroll_state"`
	Enabled         bool   `json:"enabled"`
	MaintenanceMode bool   `json:"maintenance_mode"`

	AgentVersion    string     `json:"agent_version,omitempty"`
	ProtocolVersion int        `json:"protocol_version"`
	Capabilities    []string   `json:"capabilities,omitempty"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`

	// IsMigrationTarget 表示是否可作为迁移目标节点。
	//
	// 进入维护模式时界面要提示这一条：维护中的节点**不该**继续作为迁移
	// 目标——迁移会把新虚拟机放到它上面，而那正是「引入变更」。
	IsMigrationTarget bool `json:"is_migration_target"`

	// MaintenanceReason / MaintenanceAt 仅在处于维护模式时有值。
	MaintenanceReason string     `json:"maintenance_reason,omitempty"`
	MaintenanceAt     *time.Time `json:"maintenance_at,omitempty"`

	Remark    string    `json:"remark,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EnrollToken 是一次注册令牌的签发结果。
//
// Token 是**明文**，只在这里出现一次——数据库存的是它的哈希，之后无法
// 再次取出。调用方必须当场展示给用户（拼接安装命令），不得缓存或落库。
type EnrollToken struct {
	Token     string
	ExpiresAt time.Time
	Node      View
}

// List 返回全部已接入的节点（含运行态）。
func (s *Service) List(ctx context.Context) ([]View, error) {
	var nodes []model.Node
	if err := s.db.WithContext(ctx).Order("id").Find(&nodes).Error; err != nil {
		log.Printf("[node] 查询节点列表失败: %v", err)
		return nil, api.Internal()
	}

	views := make([]View, 0, len(nodes))
	for i := range nodes {
		views = append(views, s.toView(ctx, &nodes[i]))
	}
	return views, nil
}

// Get 返回单个节点。
func (s *Service) Get(ctx context.Context, id int64) (*View, error) {
	var node model.Node
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&node).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[node] 查询节点失败: %v", err)
		return nil, api.Internal()
	}

	view := s.toView(ctx, &node)
	return &view, nil
}

// CreateEnrollToken 创建一条待接入的节点并返回一次性注册令牌。
//
// 令牌只在这里出现一次：数据库里存的是它的哈希。返回的明文由调用方
// 展示给用户（拼接安装命令），之后无法再次取出。
func (s *Service) CreateEnrollToken(
	ctx context.Context, name string, ttl time.Duration, operatorID int64, operatorName, clientIP string,
) (*EnrollToken, error) {
	name = strings.TrimSpace(name)
	if !namePattern.MatchString(name) {
		return nil, api.InvalidParameter("节点名需为 1-64 位字母、数字、点、下划线或连字符，且以字母或数字开头")
	}
	if ttl <= 0 {
		ttl = DefaultEnrollTTL
	}

	token, err := newEnrollToken()
	if err != nil {
		log.Printf("[node] 生成注册令牌失败: %v", err)
		return nil, api.Internal()
	}
	hash := hashToken(token)
	expiresAt := time.Now().Add(ttl)

	node := model.Node{
		Name:            name,
		Enabled:         true,
		Status:          model.NodeStatusUnknown,
		EnrollState:     model.NodeEnrollPending,
		EnrollTokenHash: &hash,
		EnrollExpiresAt: &expiresAt,
	}
	if err := s.db.WithContext(ctx).Create(&node).Error; err != nil {
		// 节点名唯一索引冲突是最常见的失败：给出可操作的原因，
		// 而不是笼统的「服务内部错误」。
		if isDuplicateKey(err) {
			return nil, api.Conflict("节点名已存在")
		}
		log.Printf("[node] 创建节点失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		ResourceType: "node",
		ResourceID:   node.ID,
		ResourceName: node.Name,
		Action:       "node.enroll_token.create",
		AfterState:   map[string]any{"expires_at": expiresAt},
		Success:      true,
		ClientIP:     clientIP,
	})

	return &EnrollToken{
		Token:     token,
		ExpiresAt: expiresAt,
		Node:      s.toView(ctx, &node),
	}, nil
}

// Register 用注册令牌完成 agent 注册。
//
// 真实场景下由 agent 经 gRPC 的 Bootstrap 调用；开发期可由模拟入口调用
// ——**两者共用本方法**，保证注册逻辑本身是被真实验证过的（ADR-0007）。
func (s *Service) Register(ctx context.Context, token, agentID, agentVersion string) (*View, error) {
	// 空令牌按「令牌无效」处理，而不是「参数错误」：对外表现必须与令牌
	// 不匹配完全一致，否则调用方可以据此区分二者、缩小猜测范围。
	if token == "" {
		return nil, api.PermissionDenied("注册令牌无效或已被使用")
	}
	if agentID == "" {
		return nil, api.InvalidParameter("agent 标识不能为空")
	}

	hash := hashToken(token)
	var node model.Node
	err := s.db.WithContext(ctx).
		Where("enroll_token_hash = ? AND enroll_state = ?", hash, model.NodeEnrollPending).
		First(&node).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 不区分「令牌不存在」与「已被使用」：两者对外都是「无效令牌」，
		// 区分它们只会帮助攻击者判断令牌是否曾经有效。
		return nil, api.PermissionDenied("注册令牌无效或已被使用")
	case err != nil:
		log.Printf("[node] 查询注册令牌失败: %v", err)
		return nil, api.Internal()
	}

	now := time.Now()
	if node.EnrollExpiresAt != nil && now.After(*node.EnrollExpiresAt) {
		return nil, api.PermissionDenied("注册令牌已过期，请重新生成")
	}

	updates := map[string]any{
		"agent_id":          agentID,
		"agent_version":     agentVersion,
		"enroll_state":      model.NodeEnrollEnrolled,
		"enroll_token_hash": nil, // 一次性：注册成功即销毁
		"enroll_expires_at": nil,
		"last_heartbeat_at": now,
		"last_seen_at":      now,
		"status":            model.NodeStatusOnline,
	}
	if err := s.db.WithContext(ctx).Model(&model.Node{}).
		Where("id = ?", node.ID).Updates(updates).Error; err != nil {
		log.Printf("[node] 更新节点注册状态失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		ResourceType: "node",
		ResourceID:   node.ID,
		ResourceName: node.Name,
		Action:       "node.register",
		AfterState:   map[string]any{"agent_id": agentID, "agent_version": agentVersion},
		Success:      true,
	})

	node.EnrollState = model.NodeEnrollEnrolled
	node.AgentID = &agentID
	view := s.toView(ctx, &node)
	return &view, nil
}

// Remove 移除节点。
//
// 软删除而非物理删除：审计与历史任务仍需能引用该节点。
func (s *Service) Remove(ctx context.Context, id int64, operatorID int64, operatorName, clientIP string) error {
	var node model.Node
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&node).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[node] 查询节点失败: %v", err)
		return api.Internal()
	}

	if err := s.db.WithContext(ctx).Model(&model.Node{}).Where("id = ?", id).
		Updates(map[string]any{
			"deleted_at":        time.Now(),
			"enabled":           false,
			"enroll_token_hash": nil,
			"enroll_expires_at": nil,
		}).Error; err != nil {
		log.Printf("[node] 移除节点失败: %v", err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		ResourceType: "node",
		ResourceID:   node.ID,
		ResourceName: node.Name,
		Action:       "node.remove",
		BeforeState:  map[string]any{"name": node.Name, "agent_id": node.AgentID},
		Success:      true,
		ClientIP:     clientIP,
	})
	return nil
}

// SetMaintenance 进入或退出维护模式（F-6-05）。
//
// **同步生效，不进任务队列。**
//
// 这一点与 PRD 里那句「进入与退出均为任务」不同，理由值得写清楚：
// 维护模式**纯粹是控制面的一个标志**（f-2-01 Q-009：它的意义是「不再引入
// 变更」，而非「停止业务」），节点根本不需要知道自己的这个状态——所有拦截
// 都发生在控制面受理请求的那一刻（ensureNodeUsable）。
//
// 把它做成任务会制造一个**危险的窗口**：用户在界面上看到「维护中」、
// 以为操作已经被拦住，而队列里的标志还没翻转，此时的操作依然会被受理并
// 下发到节点。这类「界面说拦住了、实际没拦住」的不一致，比多等两秒严重得多。
//
// 反过来，纯元数据的改动（备注、分组、软锁）在本项目里都是同步的，
// 这条与它们保持一致。
func (s *Service) SetMaintenance(
	ctx context.Context, id int64, enabled bool, reason string,
	operatorID int64, operatorName, clientIP string,
) (*View, error) {
	var node model.Node
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&node).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[node] 查询节点失败: %v", err)
		return nil, api.Internal()
	}

	// 未接入的节点没有维护模式可言：它上面一个虚拟机都跑不了。
	// 允许切换会给出一个「设置成功但没有任何效果」的反馈。
	if !node.IsEnrolled() {
		return nil, api.ValidationFailed("节点尚未接入，无法设置维护模式")
	}

	// 已经是目标状态就直接返回，不写库也不记审计。
	//
	// 与软锁的幂等处理一致：重复点击不是错误，但重复写一条**状态未变**的
	// 审计记录会污染流水——事后追查「什么时候进的维护模式」时，看到一串
	// 同一分钟的记录，反而说不清是哪一次真正生效的。
	if node.MaintenanceMode == enabled {
		return s.Get(ctx, id)
	}

	// 原因与时刻随状态同进同退，退出时一起清空——与业务软锁（vm_lock）
	// 同一套口径：留一个「未在维护、但原因是『升级内核』」的记录，界面
	// 要么显示一个不生效的理由，要么得写额外判断去忽略它。
	//
	// **不写进 node.remark**：那是用户自己的备注，覆盖它会让人在维护结束后
	// 发现备注已被悄悄改掉。两个不同用途的文本共用一列，代价总在事后才显现。
	updates := map[string]any{
		"maintenance_mode":   enabled,
		"maintenance_reason": nil,
		"maintenance_at":     nil,
	}
	if enabled {
		// 空原因存 NULL 而不是空串：空串在界面上会渲染成一个空的「原因：」，
		// 看起来像原因丢了；NULL 让界面能明确显示「未填写原因」。
		if r := strings.TrimSpace(reason); r != "" {
			updates["maintenance_reason"] = r
		}
		updates["maintenance_at"] = time.Now()
	}
	if err := s.db.WithContext(ctx).Model(&model.Node{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[node] 更新维护模式失败: %v", err)
		return nil, api.Internal()
	}

	action := "node.maintenance.exit"
	if enabled {
		action = "node.maintenance.enter"
	}
	s.record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		NodeID:       node.ID,
		ResourceType: "node",
		ResourceID:   node.ID,
		ResourceName: node.Name,
		Action:       action,
		Params:       map[string]any{"reason": reason},
		BeforeState:  map[string]any{"maintenance_mode": node.MaintenanceMode},
		AfterState:   map[string]any{"maintenance_mode": enabled},
		Success:      true,
		ClientIP:     clientIP,
	})

	return s.Get(ctx, id)
}

// toView 组装节点视图：元数据取自数据库，运行态取自 agent。
func (s *Service) toView(ctx context.Context, node *model.Node) View {
	snap, err := s.runtime.Snapshot(ctx, node.ID)
	if err != nil {
		// 取不到运行态不是错误：节点可能从未接入，或 agent 通道暂时不可用。
		// 这种情况显示「未知」而不是让整个列表失败。
		log.Printf("[node] 获取节点 %d 运行态失败: %v", node.ID, err)
		snap = nil
	}
	return s.viewOf(node, snap)
}

func (s *Service) viewOf(node *model.Node, snap *agent.Snapshot) View {
	view := View{
		ID:                node.ID,
		Name:              node.Name,
		EnrollState:       node.EnrollState,
		Enabled:           node.Enabled,
		MaintenanceMode:   node.MaintenanceMode,
		IsMigrationTarget: node.IsMigrationTarget,
		Status:            deriveStatus(node, snap, time.Now()),
		CreatedAt:         node.CreatedAt,
	}
	if node.Remark != nil {
		view.Remark = *node.Remark
	}
	// 只在**确实处于维护模式**时带出原因：一个「未在维护但原因是……」
	// 的响应会让界面把它显示在错误的语境里。
	if node.MaintenanceMode {
		if node.MaintenanceReason != nil {
			view.MaintenanceReason = *node.MaintenanceReason
		}
		view.MaintenanceAt = node.MaintenanceAt
	}

	// 运行态字段优先取 agent 上报；取不到时回退到数据库里缓存的上一次值，
	// 并在状态上体现为「未知」——陈旧的在线标记比没有标记更危险。
	if snap != nil {
		view.AgentVersion = snap.AgentVersion
		view.ProtocolVersion = snap.ProtocolVersion
		view.Capabilities = snap.Capabilities
		view.LastError = snap.LastError
		hb := snap.LastHeartbeat
		view.LastHeartbeatAt = &hb
		return view
	}

	if node.AgentVersion != nil {
		view.AgentVersion = *node.AgentVersion
	}
	view.ProtocolVersion = node.ProtocolVersion
	view.LastHeartbeatAt = node.LastHeartbeatAt
	view.Capabilities = decodeCapabilities(node.Capabilities)
	if node.LastError != nil {
		view.LastError = *node.LastError
	}
	return view
}

// deriveStatus 由心跳时间推导运行态。
//
// **不用 agent 上报的状态字符串**：agent 宕机时它的最后上报会停留在
// 「在线」，把「失联」显示成「正常」——而这恰恰是运维最需要立刻看出来的
// 情况。心跳时间是会自然变旧的，状态因此不会撒谎。
func deriveStatus(node *model.Node, snap *agent.Snapshot, now time.Time) string {
	if !node.IsEnrolled() {
		return model.NodeStatusUnknown
	}

	var last time.Time
	if snap != nil {
		last = snap.LastHeartbeat
	} else if node.LastHeartbeatAt != nil {
		last = *node.LastHeartbeatAt
	}
	if last.IsZero() {
		return model.NodeStatusUnknown
	}
	if now.Sub(last) > OfflineThreshold {
		return model.NodeStatusOffline
	}
	return model.NodeStatusOnline
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

// newEnrollToken 生成 24 字节随机的注册令牌（48 位十六进制）。
func newEnrollToken() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成注册令牌失败: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// hashToken 计算令牌的存储哈希。
//
// 用 SHA-256 而非 Argon2id：令牌是 24 字节的高熵随机值，不存在被暴力
// 破解的可能，因此不需要慢哈希；而注册校验位于 agent 接入的关键路径上，
// 慢哈希会带来无谓的延迟。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// decodeCapabilities 解析数据库中以 JSON 文本存储的能力清单。
func decodeCapabilities(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var caps []string
	if err := json.Unmarshal([]byte(*raw), &caps); err != nil {
		log.Printf("[node] 解析能力清单失败: %v", err)
		return nil
	}
	return caps
}

// isDuplicateKey 判断错误是否为唯一约束冲突。
//
// 不绑定具体驱动的错误类型：GORM 的 ErrDuplicatedKey 需要驱动开启翻译，
// 这里退化为字符串匹配，覆盖 PostgreSQL 与 SQLite 的常见措辞。
func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
