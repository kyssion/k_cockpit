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
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
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
}

// NewService 构造虚拟机服务。
func NewService(db *gorm.DB, queue *task.Queue, recorder *audit.Recorder) *Service {
	return &Service{db: db, queue: queue, audit: recorder}
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
	views := make([]View, 0, len(vms))
	for i := range vms {
		views = append(views, toView(&vms[i], now))
	}
	return views, total, nil
}

// Get 返回虚拟机详情；不属于当前视角的返回 404（不泄漏其是否存在）。
func (s *Service) Get(ctx context.Context, id int64, v authz.Viewer) (*View, error) {
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

	view := toView(&vm, time.Now())
	return &view, nil
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

func toView(vm *model.VM, now time.Time) View {
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
		Stale:        vm.IsStale(now, StaleThreshold),
		CreatedAt:    vm.CreatedAt,
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
