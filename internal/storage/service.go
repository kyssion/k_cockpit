// Package storage 实现存储池管理（F-5-01）。
//
// 两条贯穿本包的原则：
//   - **所有宿主侧动作由 agent 执行**（R-001）：控制面只做数据校验、下发
//     领域操作并保存元数据，不直连宿主机、不执行命令；
//   - **校验必须双端**（R-005）：控制面校验提供快速反馈，agent 校验才是
//     最终防线——控制面掌握的设备信息可能已经滞后。
package storage

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// StaleThreshold 是空间数据的陈旧阈值。
//
// 空间数据随指标周期上报而非按需探测（R-009）。显示一个陈旧容量比不显示
// 更危险：用户可能基于「还有 500G」去创建虚拟机，而实际早已写满。
const StaleThreshold = 5 * time.Minute

// globalLockResource 是所有存储池变更任务共用的资源锁键。
//
// 存储操作**全局串行**（R-003）：同一时刻只允许一个存储池变更任务运行。
// 用同一个锁键复用任务队列已有的互斥机制，而不是另建一套全局锁——多一套
// 机制就多一处可能与队列状态不一致的地方。
const globalLockResource = "storage_global"

// 支持的文件系统。
var supportedFSTypes = map[string]struct{}{
	"ext4": {}, "xfs": {}, "btrfs": {},
}

// Service 提供存储池领域操作。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	audit *audit.Recorder
	agent agent.Client
}

// NewService 构造存储池服务。
func NewService(db *gorm.DB, queue *task.Queue, recorder *audit.Recorder, client agent.Client) *Service {
	return &Service{db: db, queue: queue, audit: recorder, agent: client}
}

// DiskView 是块设备的对外视图。
type DiskView struct {
	DeviceID   string `json:"device_id"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	IsSystem   bool   `json:"is_system"`
	Mounted    bool   `json:"mounted"`
	HasData    bool   `json:"has_data"`
	Filesystem string `json:"filesystem,omitempty"`
	MountPoint string `json:"mount_point,omitempty"`

	// Usable 表示**在显式确认的前提下**可用于创建存储池。
	//
	// 有数据的盘同样 usable：它需要用户输入设备名确认，而不是被直接禁止
	// ——否则一块曾被格式化过的盘将永远无法使用。
	Usable bool `json:"usable"`
	// InUseBy 非空时是占用该设备的存储池标识，此时不可再次创建。
	InUseBy string `json:"in_use_by,omitempty"`
}

// PoolView 是存储池的对外视图。
type PoolView struct {
	ID         int64  `json:"id"`
	NodeID     int64  `json:"node_id"`
	DeviceID   string `json:"device_id"`
	DevicePath string `json:"device_path,omitempty"`
	Kind       string `json:"kind"`
	FSType     string `json:"fs_type,omitempty"`
	MountPath  string `json:"mount_path,omitempty"`

	TotalBytes  int64  `json:"total_bytes"`
	UsableBytes int64  `json:"usable_bytes"`
	IsDefault   bool   `json:"is_default"`
	Status      string `json:"status"`
	Remark      string `json:"remark,omitempty"`

	// Stale 提示空间数据可能已经过期，界面据此标注（R-009）。
	Stale          bool       `json:"stale"`
	LastReportedAt *time.Time `json:"last_reported_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Disks 返回节点的块设备清单。
//
// 每次调用都向 agent 探测，不做缓存：设备可热插拔，缓存一份清单只会在
// 设备变化时误导用户去操作一个已经不在的盘。
func (s *Service) Disks(ctx context.Context, nodeID int64) ([]DiskView, error) {
	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		return nil, err
	}

	result, err := s.agent.Execute(ctx, agent.Operation{Kind: agent.OpNodeDisks, NodeID: nodeID})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法获取磁盘清单")
	}
	if !result.Success {
		return nil, api.Unavailable("节点未返回磁盘清单")
	}

	disks, _ := result.Data[agent.DiskListKey].([]agent.Disk)

	// 一次性取出该节点已占用的设备，避免在循环里逐块查询。
	occupied, err := s.occupiedDevices(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	views := make([]DiskView, 0, len(disks))
	for _, d := range disks {
		view := DiskView{
			DeviceID:   d.DeviceID,
			Path:       d.Path,
			SizeBytes:  d.SizeBytes,
			IsSystem:   d.IsSystem,
			Mounted:    d.Mounted,
			HasData:    d.HasData,
			Filesystem: d.Filesystem,
			MountPoint: d.MountPoint,
			Usable:     d.Usable(),
		}
		if pool, ok := occupied[d.DeviceID]; ok {
			view.InUseBy = pool
			// 已被存储池占用的设备当然不能再用于创建新池。
			view.Usable = false
		}
		views = append(views, view)
	}
	return views, nil
}

// List 返回节点的存储池列表。
func (s *Service) List(ctx context.Context, nodeID int64) ([]PoolView, error) {
	query := s.db.WithContext(ctx).Model(&model.StoragePool{}).Where("deleted_at IS NULL")
	if nodeID > 0 {
		query = query.Where("node_id = ?", nodeID)
	}

	var pools []model.StoragePool
	if err := query.Order("node_id, id").Find(&pools).Error; err != nil {
		log.Printf("[storage] 查询存储池失败: %v", err)
		return nil, api.Internal()
	}

	now := time.Now()
	views := make([]PoolView, 0, len(pools))
	for i := range pools {
		views = append(views, toPoolView(&pools[i], now))
	}
	return views, nil
}

// Get 返回单个存储池。
func (s *Service) Get(ctx context.Context, id int64) (*PoolView, error) {
	pool, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	view := toPoolView(pool, time.Now())
	return &view, nil
}

// CreateRequest 是创建存储池的请求。
type CreateRequest struct {
	NodeID   int64
	DeviceID string
	FSType   string
	// IsDefault 指定是否设为该节点默认池。第一个池会被自动设为默认（R-006）。
	IsDefault bool
	// ConfirmDeviceName 是用户手工输入的设备名，须与设备路径**完全一致**
	// （R-004）。格式化不可逆，这一步的意义是让用户在动手前看清楚自己
	// 选中的是哪一块盘——仅仅点一下「我确认」起不到这个作用。
	ConfirmDeviceName string
	// ConfirmDataLoss 表示用户已明确知晓该设备上的数据将被销毁。
	// 设备已有分区表或文件系统时必须为 true，否则拒绝（§4.1）。
	ConfirmDataLoss bool
}

// Create 校验并提交创建任务。
//
// 控制面校验的顺序刻意是「越便宜越靠前」：先看节点，再看设备清单，
// 最后才是需要额外查询的占用检查。这样绝大多数非法请求在第一次调用
// agent 之前就被挡下。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, operatorID int64, operatorName, clientIP string,
) (*model.Task, error) {
	if err := s.ensureNodeUsable(ctx, req.NodeID); err != nil {
		return nil, err
	}
	if req.FSType == "" {
		req.FSType = "ext4"
	}
	if _, ok := supportedFSTypes[req.FSType]; !ok {
		return nil, api.InvalidParameter("不支持的文件系统类型")
	}

	disk, err := s.probeDisk(ctx, req.NodeID, req.DeviceID)
	if err != nil {
		return nil, err
	}

	// 设备名确认：与用户输入逐字符比较。用 TrimSpace 容忍首尾空格，
	// 但不做大小写或别名归一——设备路径是精确的东西，宽容只会让用户
	// 以为自己确认的是另一块盘。
	if strings.TrimSpace(req.ConfirmDeviceName) != disk.Path {
		return nil, api.ValidationFailed(
			"设备名确认不一致：请完整输入 " + disk.Path + " 以确认操作目标是该设备",
		)
	}
	if disk.IsSystem {
		return nil, api.ValidationFailed("该设备承载系统运行所需的分区，不能用于存储池")
	}
	if disk.Mounted {
		return nil, api.ValidationFailed("该设备或其分区处于挂载状态，请先卸载")
	}
	// 已有数据的设备必须显式确认，否则拒绝（§4.1）。
	if disk.HasData && !req.ConfirmDataLoss {
		return nil, api.ValidationFailed("该设备上已存在分区表或文件系统，请确认数据可被销毁后重试")
	}

	// 已被池占用：靠数据库唯一索引兜底，但先查一次以便给出可读的原因。
	var occupied int64
	err = s.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("node_id = ? AND device_id = ? AND deleted_at IS NULL", req.NodeID, req.DeviceID).
		Count(&occupied).Error
	if err != nil {
		log.Printf("[storage] 查询设备占用失败: %v", err)
		return nil, api.Internal()
	}
	if occupied > 0 {
		return nil, api.Conflict("该设备已被存储池占用")
	}

	isDefault, err := s.resolveDefault(ctx, req.NodeID, req.IsDefault)
	if err != nil {
		return nil, err
	}

	params := createParams{
		NodeID:     req.NodeID,
		DeviceID:   req.DeviceID,
		DevicePath: disk.Path,
		FSType:     req.FSType,
		IsDefault:  isDefault,
		HadData:    disk.HasData,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type: model.TaskStoragePoolCreate,
		// 全局锁：所有存储池变更任务串行（R-003）。
		ResourceType: globalLockResource,
		ResourceID:   1,
		ResourceName: disk.Path,
		NodeID:       req.NodeID,
		// 存储池是**节点级资源**，不归属某个用户：记 OwnerID 会让 tenant
		// 通过任务列表看到与自己无关的存储操作。
		CreatedBy: operatorID,
		Params:    params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		NodeID:       req.NodeID,
		ResourceType: "storage_pool",
		ResourceName: disk.Path,
		Action:       "storage.pool.create.request",
		Params:       params,
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// Delete 校验并提交删除任务。
//
// **删除前必须做占用检查**（R-008）：池内若存在被虚拟机使用的磁盘，拒绝
// 删除并列出占用者。占用情况只有 agent 清楚（虚拟机磁盘是按路径归属的），
// 因此这里向节点探测一次，而不是凭控制面记录猜。
func (s *Service) Delete(
	ctx context.Context, id int64, operatorID int64, operatorName, clientIP string,
) (*model.Task, error) {
	pool, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, pool.NodeID); err != nil {
		return nil, err
	}

	// 节点不可达时拒绝删除，而不是盲目下发：无法确认占用情况就删除，
	// 可能删掉正在被虚拟机使用的池（边界表：节点离线 → 拒绝）。
	volumes, err := s.scanVolumes(ctx, pool)
	if err != nil {
		return nil, err
	}
	if len(volumes) > 0 {
		return nil, api.Conflict(
			"该存储池内仍存在磁盘：" + strings.Join(volumes, "、") + "，请先删除占用它们的虚拟机",
		)
	}

	params := deleteParams{
		PoolID:     pool.ID,
		NodeID:     pool.NodeID,
		DeviceID:   pool.DeviceID,
		DevicePath: derefStr(pool.DevicePath),
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStoragePoolDelete,
		ResourceType: globalLockResource,
		ResourceID:   1,
		ResourceName: derefStr(pool.DevicePath),
		NodeID:       pool.NodeID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		NodeID:       pool.NodeID,
		ResourceType: "storage_pool",
		ResourceID:   pool.ID,
		ResourceName: derefStr(pool.DevicePath),
		Action:       "storage.pool.delete.request",
		Params:       params,
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// SetDefault 切换默认池。
//
// 同步完成而不入队：它只改控制面的元数据，不触碰宿主机，没有耗时可言。
// 把它做成异步任务只会让用户点一下「设为默认」还要去任务中心看结果。
func (s *Service) SetDefault(
	ctx context.Context, id int64, isDefault bool, operatorName, clientIP string,
) (*PoolView, error) {
	pool, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if isDefault {
			// 先清掉同节点的其他默认池：数据库的部分唯一索引会拒绝两个
			// 默认池，但让它在事务里失败会让用户看到一句约束冲突。
			if err := tx.Model(&model.StoragePool{}).
				Where("node_id = ? AND is_default = ?", pool.NodeID, true).
				Update("is_default", false).Error; err != nil {
				return err
			}
		}
		return tx.Model(&model.StoragePool{}).Where("id = ?", pool.ID).
			Update("is_default", isDefault).Error
	})
	if err != nil {
		log.Printf("[storage] 切换默认池失败 id=%d: %v", pool.ID, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorName: operatorName,
		NodeID:       pool.NodeID,
		ResourceType: "storage_pool",
		ResourceID:   pool.ID,
		ResourceName: derefStr(pool.DevicePath),
		Action:       "storage.pool.set_default",
		AfterState:   map[string]any{"is_default": isDefault},
		Success:      true,
		ClientIP:     clientIP,
	})

	updated, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	view := toPoolView(updated, time.Now())
	return &view, nil
}

// load 读取未删除的存储池。
func (s *Service) load(ctx context.Context, id int64) (*model.StoragePool, error) {
	var pool model.StoragePool
	err := s.db.WithContext(ctx).
		Where("id = ? AND deleted_at IS NULL", id).
		First(&pool).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("存储池不存在")
	case err != nil:
		log.Printf("[storage] 查询存储池失败: %v", err)
		return nil, api.Internal()
	}
	return &pool, nil
}

// ensureNodeUsable 校验节点存在且可用于存储操作。
func (s *Service) ensureNodeUsable(ctx context.Context, nodeID int64) error {
	var n model.Node
	err := s.db.WithContext(ctx).
		Select("id", "maintenance_mode", "enabled").
		Where("id = ?", nodeID).
		First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("节点不存在")
	case err != nil:
		log.Printf("[storage] 查询节点失败: %v", err)
		return api.Internal()
	}
	// 维护模式下不允许存储变更：格式化与卸载都属于「引入变更」（R-014 同理）。
	if n.MaintenanceMode {
		return api.ValidationFailed("节点处于维护模式，已暂停存储池变更")
	}
	return nil
}

// probeDisk 从节点的最新清单中找出指定设备。
//
// 设备不在清单中即拒绝：控制面记录里的设备可能已经被拔掉，凭旧记录去
// 格式化一块不存在的盘，结果只会是一个含义不明的失败。
func (s *Service) probeDisk(ctx context.Context, nodeID int64, deviceID string) (*agent.Disk, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{Kind: agent.OpNodeDisks, NodeID: nodeID})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法校验设备")
	}
	if !result.Success {
		return nil, api.Unavailable("节点未返回磁盘清单")
	}

	disks, _ := result.Data[agent.DiskListKey].([]agent.Disk)
	for i := range disks {
		if disks[i].DeviceID == deviceID {
			return &disks[i], nil
		}
	}
	return nil, api.ValidationFailed("该设备不在节点当前上报的磁盘清单中")
}

// scanVolumes 探测存储池内的磁盘。
func (s *Service) scanVolumes(ctx context.Context, pool *model.StoragePool) ([]string, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpStoragePoolScan,
		NodeID: pool.NodeID,
		Target: derefStr(pool.DevicePath),
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法确认存储池占用情况")
	}
	if !result.Success {
		return nil, api.Unavailable("无法确认存储池占用情况")
	}

	volumes, _ := result.Data[agent.VolumeListKey].([]string)
	return volumes, nil
}

// occupiedDevices 返回该节点已被存储池占用的设备。
func (s *Service) occupiedDevices(ctx context.Context, nodeID int64) (map[string]string, error) {
	var pools []model.StoragePool
	err := s.db.WithContext(ctx).
		Select("id", "device_id", "device_path").
		Where("node_id = ? AND deleted_at IS NULL", nodeID).
		Find(&pools).Error
	if err != nil {
		log.Printf("[storage] 查询已占用设备失败: %v", err)
		return nil, api.Internal()
	}

	occupied := make(map[string]string, len(pools))
	for _, p := range pools {
		label := derefStr(p.DevicePath)
		if label == "" {
			label = p.DeviceID
		}
		occupied[p.DeviceID] = label
	}
	return occupied, nil
}

// resolveDefault 决定新池是否设为默认。
//
// 第一个池自动成为默认（R-006）——否则用户创建完池还要再去点一次「设为
// 默认」，而绝大多数节点只有一个池。
func (s *Service) resolveDefault(ctx context.Context, nodeID int64, requested bool) (bool, error) {
	if requested {
		return true, nil
	}

	var count int64
	err := s.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("node_id = ? AND deleted_at IS NULL", nodeID).
		Count(&count).Error
	if err != nil {
		log.Printf("[storage] 统计存储池失败: %v", err)
		return false, api.Internal()
	}
	return count == 0, nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

func toPoolView(p *model.StoragePool, now time.Time) PoolView {
	return PoolView{
		ID:             p.ID,
		NodeID:         p.NodeID,
		DeviceID:       p.DeviceID,
		DevicePath:     derefStr(p.DevicePath),
		Kind:           p.Kind,
		FSType:         derefStr(p.FSType),
		MountPath:      derefStr(p.MountPath),
		TotalBytes:     p.TotalBytes,
		UsableBytes:    p.UsableBytes,
		IsDefault:      p.IsDefault,
		Status:         p.Status,
		Remark:         derefStr(p.Remark),
		Stale:          p.IsStale(now, StaleThreshold),
		LastReportedAt: p.LastReportedAt,
		CreatedAt:      p.CreatedAt,
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
