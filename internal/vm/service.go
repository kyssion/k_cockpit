// Package vm 实现虚拟机管理（F-2-01 ~ F-2-04）。
//
// 数据分两类（见 model.VM 的注释）：投影字段的权威在虚拟化层，元数据字段的
// 权威在控制面。本包负责读写这两类数据，而**真正的操作经任务队列下发给
// agent**——接口不等待执行完成（f-7-01 R-001）。
package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/settings"
	"k_cockpit/internal/task"
)

// StaleThreshold 是投影数据的陈旧阈值。
//
// 超过该时长未与虚拟化层对账时，界面应提示「数据可能陈旧」——
// 把陈旧数据显示成当前状态，在排障时比没有数据更危险（f-2-01 Q-006）。
const StaleThreshold = 60 * time.Second

// namePattern 限定虚拟机名：与虚拟化层的命名约束保持一致。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

// Service 提供虚拟机领域操作。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	audit *audit.Recorder
	// agent 用于**实时探测**运行态：投影不得参与业务判定（f-2-01 R-002），
	// 因此受理写操作前必须向节点确认一次真实状态，而不是凭投影下结论。
	agent agent.Client
	// settings 用于读取可调整的运行参数（f-9-01）。为 nil 时使用内置默认值，
	// 这样单测与轻量部署不必先装配设置模块。
	settings settings.Provider
	// encKey 用于加解密控制台密码等可逆凭据（f-2-08）。
	encKey []byte

	// sessions 是控制台会话注册表。惰性创建：不用控制台的服务实例
	// 不必为此分配内存。
	sessions     *sessionRegistry
	sessionsOnce sync.Once
}

// SetEncryptionKey 设置可逆凭据的加密密钥。
func (s *Service) SetEncryptionKey(key []byte) {
	s.encKey = key
}

// NewService 构造虚拟机服务。
func NewService(
	db *gorm.DB, queue *task.Queue, recorder *audit.Recorder, client agent.Client,
	provider settings.Provider,
) *Service {
	return &Service{db: db, queue: queue, audit: recorder, agent: client, settings: provider}
}

// staleThreshold 返回当前的投影陈旧阈值。
//
// 从设置读取而不是直接用常量：这个值写进规格时是 60 秒，但不同部署环境
// 对「多久算陈旧」的容忍度不同——节点多、心跳慢的环境需要放宽它。
func (s *Service) staleThreshold() time.Duration {
	if s.settings == nil {
		return StaleThreshold
	}
	seconds := s.settings.Int(settings.KeyVMStaleThreshold, int(StaleThreshold.Seconds()))
	if seconds <= 0 {
		return StaleThreshold
	}
	return time.Duration(seconds) * time.Second
}

// View 是虚拟机的对外视图。
type View struct {
	ID      int64  `json:"id"`
	NodeID  int64  `json:"node_id"`
	Name    string `json:"name"`
	UUID    string `json:"uuid,omitempty"`
	OwnerID *int64 `json:"owner_id,omitempty"`

	Status    string `json:"status"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	IPSummary string `json:"ip_summary,omitempty"`

	Remark    string `json:"remark,omitempty"`
	GroupName string `json:"group_name,omitempty"`

	Present      bool       `json:"present"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	// Stale 提示投影数据可能已经过期，界面据此展示「数据可能陈旧」。
	Stale     bool      `json:"stale"`
	CreatedAt time.Time `json:"created_at"`

	// AvailableActions 是按**投影状态**算出的可用电源操作，供界面渲染按钮。
	//
	// 它可能与实际可用性不一致（投影滞后），此时后端会在受理请求时基于
	// 实时探测拒绝并说明原因。前端禁用只是体验优化，不构成安全边界
	// （f-2-01 R-004）。`stale` 为 true 时界面不应完全依赖它。
	AvailableActions []string `json:"available_actions"`

	// HasConsole 表示该虚拟机是否有可用的控制台（display != none）。
	//
	// 为 false 时界面应隐藏控制台入口，而不是给一个点了打不开的按钮
	// （f-2-08 R-011）。
	HasConsole bool `json:"has_console"`

	// Locked 表示该虚拟机被业务软锁保护（F-2-12），此时禁止删除。
	//
	// 它由**后端算好下发**，界面不自行判断：批量操作要提前提示「其中 N 台
	// 已锁定」（f-2-01 R-010），而前端的判断依据只能来自列表接口本身——
	// 让每个页面各自再查一次锁定状态，迟早会出现「界面上没标锁定、点删除
	// 却被拒绝」的不一致。
	Locked bool `json:"locked"`
	// LockReason 是加锁时填写的原因，供界面解释「为什么锁着」。
	LockReason string     `json:"lock_reason,omitempty"`
	LockedAt   *time.Time `json:"locked_at,omitempty"`

	// RescueActive 表示该虚拟机当前从救援镜像启动（F-2-12）。
	//
	// 界面据此显示醒目提示：救援模式下看到的系统**不是用户自己的系统**
	// （盘型、网卡、引导顺序都改过），把它当成日常状态会让人做出错误判断。
	RescueActive bool       `json:"rescue_active"`
	RescueSince  *time.Time `json:"rescue_since,omitempty"`
}

// ListFilter 是列表查询条件。
type ListFilter struct {
	Status    string
	Keyword   string
	NodeID    int64
	GroupName string
	Page      int
	PageSize  int
	Viewer    authz.Viewer
}

// List 返回虚拟机列表与总数。
//
// 总数是**归属过滤后**的数量：返回全局总数会让 tenant 通过翻页差异推断出
// 他人有多少虚拟机（f-1-06 §5.2）。
func (s *Service) List(ctx context.Context, f ListFilter) ([]View, int64, error) {
	page, pageSize := normalizePage(f.Page, f.PageSize)

	query := s.db.WithContext(ctx).Model(&model.VM{})
	query = applyFilter(query, f)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		log.Printf("[vm] 统计虚拟机失败: %v", err)
		return nil, 0, api.Internal()
	}

	var vms []model.VM
	if err := query.Order("id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&vms).Error; err != nil {
		log.Printf("[vm] 查询虚拟机失败: %v", err)
		return nil, 0, api.Internal()
	}

	now := time.Now()
	// 阈值只读一次：放在循环里会让每台虚拟机都触发一次设置查询，
	// 而列表页可能有上百台。
	threshold := s.staleThreshold()

	// 锁定状态同样**一次查完**：列表页上要标出哪些机器被锁着，
	// 逐个查会把一次列表请求变成上百次数据库往返。
	ids := make([]int64, 0, len(vms))
	for i := range vms {
		ids = append(ids, vms[i].ID)
	}
	locks, err := s.lockMap(ctx, ids)
	if err != nil {
		log.Printf("[vm] 查询锁定状态失败: %v", err)
		return nil, 0, api.Internal()
	}

	views := make([]View, 0, len(vms))
	for i := range vms {
		views = append(views, toView(&vms[i], locks[vms[i].ID], now, threshold))
	}
	return views, total, nil
}

// Get 返回虚拟机详情；不属于当前视角的返回 404（不泄漏其是否存在）。
func (s *Service) Get(ctx context.Context, id int64, v authz.Viewer) (*View, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	lock, err := s.LockOf(ctx, id)
	if err != nil {
		return nil, err
	}

	view := toView(vm, lock, time.Now(), s.staleThreshold())
	return &view, nil
}

// load 按归属读取虚拟机。
//
// 不属于当前视角的返回 **404 而非 403**：403 会告诉调用方「这个 ID 确实
// 存在，只是你没权限」，从而可以被用来枚举他人资源（f-1-06 §5.2）。
func (s *Service) load(ctx context.Context, id int64, v authz.Viewer) (*model.VM, error) {
	query := s.db.WithContext(ctx).Where("id = ?", id)
	if !v.IsAdmin {
		query = query.Where("owner_id = ?", v.UserID)
	}

	var vm model.VM
	err := query.First(&vm).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("虚拟机不存在")
	case err != nil:
		log.Printf("[vm] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	return &vm, nil
}

// CreateRequest 是创建虚拟机的请求。
type CreateRequest struct {
	Name      string
	NodeID    int64
	VCPU      int
	MemoryMB  int
	DiskGB    int
	Remark    string
	GroupName string
}

// Create 入队一个创建任务并立即返回。
//
// **不等待创建完成**：创建虚拟机涉及磁盘镜像复制等耗时操作，同步等待会让
// 请求超时，也会让用户在界面上干等（f-7-01 R-001）。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, owner authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	req.Name = strings.TrimSpace(req.Name)
	if !namePattern.MatchString(req.Name) {
		return nil, api.InvalidParameter("虚拟机名需为 1-63 位字母、数字或连字符，且以字母或数字开头")
	}
	if req.NodeID <= 0 {
		return nil, api.InvalidParameter("必须指定节点")
	}
	if req.VCPU <= 0 || req.MemoryMB <= 0 || req.DiskGB <= 0 {
		return nil, api.InvalidParameter("CPU、内存与磁盘必须为正数")
	}

	params := createParams{
		Name:      req.Name,
		NodeID:    req.NodeID,
		VCPU:      req.VCPU,
		MemoryMB:  req.MemoryMB,
		DiskGB:    req.DiskGB,
		Remark:    req.Remark,
		GroupName: req.GroupName,
		OwnerID:   owner.UserID,
	}

	// 幂等键由业务语义构成：同名同节点的创建意图只应产生一个任务。
	// 用时间戳或随机数做键等于没有幂等——重复点击会创建出多台虚拟机。
	idempotencyKey := fmt.Sprintf("%s:%d:%s", model.TaskVMCreate, req.NodeID, req.Name)

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:           model.TaskVMCreate,
		NodeID:         req.NodeID,
		ResourceType:   "node",
		ResourceID:     req.NodeID,
		ResourceName:   req.Name,
		OwnerID:        owner.UserID,
		CreatedBy:      owner.UserID,
		Params:         params,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   owner.UserID,
		OperatorName: operatorName,
		NodeID:       req.NodeID,
		ResourceType: "vm",
		ResourceName: req.Name,
		Action:       "vm.create.request",
		Params:       params,
		AfterState:   map[string]any{"task_id": t.ID},
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// 磁盘处理方式（f-2-01 R-009）。
const (
	DiskActionDelete = "delete"
	DiskActionKeep   = "keep"
)

// Power 受理一次电源操作。
//
// 受理前做三件事，缺一不可：
//  1. **归属校验**——不属于当前视角的返回 404，不泄漏资源是否存在；
//  2. **维护模式校验**——维护模式的意义是「不再引入变更」（R-014）；
//  3. **实时探测 + 状态机校验**——投影可能滞后，凭它判断就可能在虚拟机
//     实际运行时执行危险操作（R-002 / R-004）。
//
// 通过后入队并立即返回：真正的执行由任务队列按资源锁串行（R-005），
// 同一虚拟机的并发电源操作不会交错。
//
// 刻意**不设幂等键**：连点两次「关机」会产生两个任务，第二个执行时因
// 状态已变而失败并说明原因。这是对的——用固定的幂等键（如
// `vm.power:{id}:start`）会把「开机→关机→再开机」中第三次开机与第一次
// 判为同一意图而静默丢弃。
func (s *Service) Power(
	ctx context.Context, id int64, rawAction string, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	action, err := ParsePowerAction(rawAction)
	if err != nil {
		return nil, err
	}

	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	if err := action.Validate(current); err != nil {
		return nil, err
	}

	params := powerParams{
		VMID:   vm.ID,
		VMName: vm.Name,
		Action: string(action),
		// 记录受理时的真实状态，便于事后区分「探测结果与投影不一致」
		// 与「执行时状态已变」两种情况。
		ObservedStatus: current,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMPower,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		Action:       "vm.power.request",
		Params:       params,
		BeforeState:  map[string]any{"status": current},
		AfterState:   map[string]any{"task_id": t.ID},
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// DeleteRequest 是一次删除请求。
type DeleteRequest struct {
	// DiskAction 必填，取值 delete / keep。
	DiskAction string
}

// Delete 受理一次删除。
//
// 磁盘处理方式**必填且不做默认**（R-009）：默认值即「用户最可能接受的
// 选项」，若默认连盘删除，误操作代价是数据永久丢失；默认保留会累积无主
// 磁盘，但可以事后清理——两者相较取其轻，因此把选择交给用户显式做出。
func (s *Service) Delete(
	ctx context.Context, id int64, req DeleteRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	switch req.DiskAction {
	case DiskActionDelete, DiskActionKeep:
	default:
		return nil, api.InvalidParameter("必须选择磁盘处理方式：delete（连同磁盘删除）或 keep（保留磁盘）")
	}

	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 锁定检查放在最前面（f-2-01 R-010 / f-2-12）。
	//
	// 它是所有拒绝理由里**唯一一个持久且可自解**的：在途任务等一会儿就没了、
	// 运行态关个机就好，而锁必须由用户主动解开。先告诉他能立刻解决的那一条，
	// 比让他先等任务跑完、再发现还锁着要好。
	if err := s.ensureNotLocked(ctx, vm); err != nil {
		return nil, err
	}

	// 在途任务存在时拒绝删除，而不是排队（f-2-01 边界）。
	//
	// 与电源操作不同：电源操作排队是合理的（用户可能连续调整），而删除
	// 排在创建/开机后面执行，意味着「用户以为取消了的操作其实照样做了」。
	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	// 删除同样要探测：对运行中的虚拟机执行删除会强杀来宾进程并删除磁盘，
	// 代价不可逆。
	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	switch current {
	case model.VMStatusRunning, model.VMStatusPaused, model.VMStatusSuspended:
		return nil, api.ValidationFailed(
			"虚拟机当前为" + DescribeStatus(current) + "，请先关机或强制断电后再删除",
		)
	}

	params := deleteParams{
		VMID:           vm.ID,
		VMName:         vm.Name,
		DiskAction:     req.DiskAction,
		ObservedStatus: current,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDelete,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		Action:       "vm.delete.request",
		Params:       params,
		BeforeState:  map[string]any{"status": current},
		AfterState:   map[string]any{"task_id": t.ID, "disk_action": req.DiskAction},
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// probeStatus 向节点**实时探测**虚拟机的真实运行态。
//
// 投影不可用于业务判定（R-002），因此每个写操作受理前都要走这一趟。
// 探测失败一律拒绝操作：无法确认状态时不猜，猜错的方向可能是对运行中的
// 虚拟机断电。
func (s *Service) probeStatus(ctx context.Context, vm *model.VM) (string, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMStatus,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil {
		return "", api.Unavailable("节点不可达，无法确认虚拟机当前状态")
	}
	if !result.Success {
		return "", api.Unavailable("无法确认虚拟机当前状态")
	}

	status, _ := result.Data[agent.StatusDataKey].(string)
	if status == "" {
		return "", api.Unavailable("节点未返回虚拟机状态")
	}
	return status, nil
}

// ensureNodeUsable 校验节点存在且未处于维护模式。
func (s *Service) ensureNodeUsable(ctx context.Context, nodeID int64) error {
	var n model.Node
	err := s.db.WithContext(ctx).
		Select("id", "maintenance_mode").
		Where("id = ?", nodeID).
		First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.ValidationFailed("虚拟机的所属节点已不存在")
	case err != nil:
		log.Printf("[vm] 查询节点失败: %v", err)
		return api.Internal()
	}
	if n.MaintenanceMode {
		return api.ValidationFailed("节点处于维护模式，已暂停创建与电源操作")
	}
	return nil
}

// hasActiveTask 报告该虚拟机是否有在途任务。
//
// unknown 也算在途：它等待节点重连后对账收敛，而不是已经结束。
func (s *Service) hasActiveTask(ctx context.Context, vmID int64) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&model.Task{}).
		Where("resource_type = ? AND resource_id = ?", "vm", vmID).
		Where("status IN ?", []string{model.TaskPending, model.TaskRunning, model.TaskUnknown}).
		Count(&count).Error
	if err != nil {
		log.Printf("[vm] 统计在途任务失败: %v", err)
		return false, api.Internal()
	}
	return count > 0, nil
}

// ownerOf 返回新任务应记录的归属。
//
// 管理员代他人操作时沿用资源原有的归属，否则会把别人的虚拟机「过户」给自己
// ——那是权限提升的一个入口（f-1-06 R-007）。
func ownerOf(vm *model.VM, v authz.Viewer) int64 {
	if vm.OwnerID != nil {
		return *vm.OwnerID
	}
	return v.UserID
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

func applyFilter(query *gorm.DB, f ListFilter) *gorm.DB {
	// 已不在虚拟化层的记录默认不显示，但保留在库中供审计与历史引用。
	query = query.Where("present = ?", true)

	if f.Status != "" {
		query = query.Where("status = ?", f.Status)
	}
	if f.NodeID > 0 {
		query = query.Where("node_id = ?", f.NodeID)
	}
	if f.GroupName != "" {
		query = query.Where("group_name = ?", f.GroupName)
	}
	if f.Keyword != "" {
		// 转义 LIKE 通配符：否则用户输入一个 % 就会匹配到全部记录，
		// 看起来像「搜索功能坏了」。
		keyword := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(f.Keyword)
		// 用 LOWER(...) LIKE LOWER(...) 而非 ILIKE：后者是 PostgreSQL 专有语法，
		// 在 SQLite（单元测试与轻量部署）上会直接报错——查询行为会随驱动而变，
		// 而这类差异只会在切换数据库时才暴露。
		query = query.Where("LOWER(name) LIKE LOWER(?)", "%"+keyword+"%")
	}
	// 归属过滤强制注入：漏加一处就是一次越权。
	if !f.Viewer.IsAdmin {
		query = query.Where("owner_id = ?", f.Viewer.UserID)
	}
	return query
}

// toView 构造对外视图。
//
// lock 允许为 nil（未加锁，且多数虚拟机连记录都没有）。
func toView(vm *model.VM, lock *model.VMLock, now time.Time, threshold time.Duration) View {
	view := View{
		ID:           vm.ID,
		NodeID:       vm.NodeID,
		Name:         vm.Name,
		OwnerID:      vm.OwnerID,
		Status:       vm.Status,
		VCPU:         vm.VCPU,
		MemoryMB:     vm.MemoryMB,
		DiskGB:       vm.DiskGB,
		Present:      vm.Present,
		LastSyncedAt: vm.LastSyncedAt,
		Stale:        vm.IsStale(now, threshold),
		CreatedAt:    vm.CreatedAt,
		// 按投影状态给出可用动作。投影滞后时可能与实际不符，后端受理时
		// 会以实时探测为准重新校验（f-2-01 R-004）。
		AvailableActions: AvailableActions(vm.Status),
		HasConsole:       vm.HasConsole(),
		RescueActive:     vm.RescueActive,
		RescueSince:      vm.RescueSince,
	}
	if vm.UUID != nil {
		view.UUID = *vm.UUID
	}
	if vm.IPSummary != nil {
		view.IPSummary = *vm.IPSummary
	}
	if vm.Remark != nil {
		view.Remark = *vm.Remark
	}
	if vm.GroupName != nil {
		view.GroupName = *vm.GroupName
	}
	// lock 允许为 nil：绝大多数虚拟机没有加过锁，连一行记录都没有。
	// 让调用方去构造一个空记录只会在每个调用点重复同一段判断。
	if lock.IsLocked() {
		view.Locked = true
		view.LockReason = derefStr(lock.Reason)
		view.LockedAt = lock.LockedAt
	}
	return view
}

func normalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
