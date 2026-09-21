package storage

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// --- 分区 ---

// PartitionView 是一个分区的对外形态。
type PartitionView struct {
	Index       int    `json:"index"`
	Path        string `json:"path"`
	SizeBytes   int64  `json:"size_bytes"`
	FSType      string `json:"fs_type,omitempty"`
	Mounted     bool   `json:"mounted"`
	System      bool   `json:"system"`
	InUseByPool string `json:"in_use_by_pool,omitempty"`
}

// PartitionListView 是一块磁盘上的分区情况。
type PartitionListView struct {
	NodeID   int64           `json:"node_id"`
	DeviceID string          `json:"device_id"`
	Items    []PartitionView `json:"items"`
	// Unavailable 非空表示节点未提供分区信息。
	Unavailable string `json:"unavailable,omitempty"`
}

// Partitions 读取一块磁盘上的分区。
func (s *Service) Partitions(ctx context.Context, nodeID int64, deviceID string) (*PartitionListView, error) {
	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		return nil, err
	}
	out := &PartitionListView{NodeID: nodeID, DeviceID: deviceID, Items: []PartitionView{}}
	if strings.TrimSpace(deviceID) == "" {
		return nil, api.InvalidParameter("必须指定磁盘")
	}
	if s.agent == nil {
		out.Unavailable = "未连接节点"
		return out, nil
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpStoragePartitions,
		NodeID: nodeID,
		Target: deviceID,
	})
	if err != nil || result == nil || !result.Success || result.Data == nil {
		out.Unavailable = "节点未提供分区信息"
		return out, nil
	}
	rows := decodePartitions(result.Data)
	if len(rows) == 0 {
		out.Unavailable = "节点未提供分区信息"
		return out, nil
	}
	for i := range rows {
		out.Items = append(out.Items, PartitionView{
			Index: rows[i].Index, Path: rows[i].Path, SizeBytes: rows[i].SizeBytes,
			FSType: rows[i].FSType, Mounted: rows[i].Mounted, System: rows[i].System,
			InUseByPool: rows[i].InUseByPool,
		})
	}
	return out, nil
}

// PartitionRequest 是一次分区操作。
type PartitionRequest struct {
	NodeID   int64
	DeviceID string
	// SizeGB 是要创建的分区大小（GB）。
	SizeGB int
	// All 为 true 时删除该磁盘上的**全部**分区。
	All bool
	// Index 指定要删除的分区号；All 为 true 时忽略。
	Index int
}

// CreatePartition 在空闲空间上创建分区。
//
// 走任务队列：改分区表是一个会**影响整块磁盘**的动作，中途失败需要节点
// 自己收敛（重读分区表），而同步接口里没有"我做到哪一步了"这个返回值。
func (s *Service) CreatePartition(
	ctx context.Context, req PartitionRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	if err := s.ensureNodeUsable(ctx, req.NodeID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.DeviceID) == "" || req.SizeGB <= 0 {
		return nil, api.InvalidParameter("必须指定磁盘与分区大小")
	}
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStoragePartitionCreate,
		NodeID:       req.NodeID,
		ResourceType: globalLockResource,
		ResourceName: req.DeviceID,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: partitionParams{
			NodeID: req.NodeID, DeviceID: req.DeviceID, SizeGB: req.SizeGB,
		},
	})
	if err != nil {
		return nil, err
	}
	s.auditAction(ctx, v.UserID, operatorName, clientIP, "storage.partition.create", req.NodeID, 0, true, "task:"+strconv.FormatInt(t.ID, 10))
	return t, nil
}

// DeletePartitions 删除分区。
//
// 删除**全部**分区必须显式传 All：一次点击抹掉整张分区表与"删掉第三个"
// 不是一个量级的动作，靠参数长度去猜用户意图是最容易出错的一类设计。
func (s *Service) DeletePartitions(
	ctx context.Context, req PartitionRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	if err := s.ensureNodeUsable(ctx, req.NodeID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.DeviceID) == "" {
		return nil, api.InvalidParameter("必须指定磁盘")
	}
	if !req.All && req.Index <= 0 {
		return nil, api.InvalidParameter("必须指定分区号，或明确删除全部")
	}
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStoragePartitionDelete,
		NodeID:       req.NodeID,
		ResourceType: globalLockResource,
		ResourceName: req.DeviceID,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: partitionParams{
			NodeID: req.NodeID, DeviceID: req.DeviceID, Index: req.Index, All: req.All,
		},
	})
	if err != nil {
		return nil, err
	}
	s.auditAction(ctx, v.UserID, operatorName, clientIP, "storage.partition.delete", req.NodeID, 0, true, "task:"+strconv.FormatInt(t.ID, 10))
	return t, nil
}

// --- 池配置 ---

// PoolConfigRequest 是要下发的池配置。
type PoolConfigRequest struct {
	// MountPath 为 nil 表示不改挂载点。
	MountPath *string
	// AutoMount 为 nil 表示不改开机自动挂载。
	AutoMount *bool
	// Remark 是控制面备注，不需要下发。
	Remark *string
}

// PoolConfigResultView 是池配置下发的结果。
type PoolConfigResultView struct {
	PoolID    int64  `json:"pool_id"`
	MountPath string `json:"mount_path"`
	AutoMount bool   `json:"auto_mount"`
	Message   string `json:"message"`
}

// UpdatePoolConfig 下发池配置。
//
// 它走任务队列，而 PATCH（设为默认）是同步的：前者要动宿主机上的挂载与
// fstab，后者只是改控制面的一列。把两者混进同一个"修改"接口会让"保存"在
// 不同字段上有完全不同的耗时与失败语义——用户没法从界面上预知。
func (s *Service) UpdatePoolConfig(
	ctx context.Context, id int64, req PoolConfigRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	pool, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.MountPath != nil {
		p := strings.TrimSpace(*req.MountPath)
		if p == "" {
			return nil, api.InvalidParameter("挂载点不能为空")
		}
		if !strings.HasPrefix(p, "/") {
			return nil, api.InvalidParameter("挂载点必须是绝对路径")
		}
		req.MountPath = &p
	}

	// 控制面先记下意图：即使节点还没执行完，界面也应显示"我们期望它是什么"。
	updates := map[string]any{"updated_at": time.Now()}
	if req.AutoMount != nil {
		updates["auto_mount"] = *req.AutoMount
	}
	if req.Remark != nil {
		updates["remark"] = req.Remark
	}
	if err := s.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[storage] 更新池配置失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}

	spec := map[string]any{}
	if req.MountPath != nil {
		spec["mount_path"] = *req.MountPath
	}
	if req.AutoMount != nil {
		spec["auto_mount"] = *req.AutoMount
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStoragePoolConfig,
		NodeID:       pool.NodeID,
		ResourceType: globalLockResource,
		ResourceID:   pool.ID,
		ResourceName: derefStr(pool.DevicePath),
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: poolConfigParams{
			PoolID: pool.ID, DeviceID: pool.DeviceID, Spec: spec,
		},
	})
	if err != nil {
		return nil, err
	}
	s.auditAction(ctx, v.UserID, operatorName, clientIP, "storage.pool.config", pool.NodeID, pool.ID, true, "task:"+strconv.FormatInt(t.ID, 10))
	return t, nil
}

// --- 卸载 ---

// UnmountPool 卸载存储池（保留数据）。
//
// 它与"删除"是两件不同的事：删除会清数据，卸载只是摘掉挂载并清除开机
// 挂载项。混淆两者的后果是——用户想临时停用一块盘，结果整池数据没了。
func (s *Service) UnmountPool(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	pool, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStoragePoolUnmount,
		NodeID:       pool.NodeID,
		ResourceType: globalLockResource,
		ResourceID:   pool.ID,
		ResourceName: derefStr(pool.DevicePath),
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: poolConfigParams{
			PoolID: pool.ID, DeviceID: pool.DeviceID,
		},
	})
	if err != nil {
		return nil, err
	}
	s.auditAction(ctx, v.UserID, operatorName, clientIP, "storage.pool.unmount", pool.NodeID, pool.ID, true, "task:"+strconv.FormatInt(t.ID, 10))
	return t, nil
}

// --- trim ---

// TrimResultView 是 trim 的结果。
type TrimResultView struct {
	NodeID         int64  `json:"node_id"`
	Devices        int    `json:"devices"`
	ReclaimedBytes int64  `json:"reclaimed_bytes"`
	Message        string `json:"message"`
}

// TrimStorage 对节点上的块设备下发 trim / discard。
//
// 它存在的意义是**回收空间**：虚拟磁盘删掉之后，宿主机上的块设备往往还
// 占着那些块（尤其是 SSD 上的 discard 未下发时）。没有它，"删了 200 GB
// 但可用空间没变"会成为一类无法解释的问题。
func (s *Service) TrimStorage(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*TrimResultView, error) {
	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		return nil, err
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点")
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpStorageTrim,
		NodeID: nodeID,
		Target: "storage",
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，trim 未执行")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info := decodeTrim(result.Data)
	s.auditAction(ctx, v.UserID, operatorName, clientIP, "storage.trim", nodeID, 0, true, firstNonEmpty(info.Message, "trim 已完成"))
	return &TrimResultView{
		NodeID:         nodeID,
		Devices:        info.Devices,
		ReclaimedBytes: info.ReclaimedBytes,
		Message:        firstNonEmpty(info.Message, "trim 已完成"),
	}, nil
}

// --- 任务参数与解码 ---

type partitionParams struct {
	NodeID   int64  `json:"node_id"`
	DeviceID string `json:"device_id"`
	SizeGB   int    `json:"size_gb"`
	Index    int    `json:"index"`
	All      bool   `json:"all"`
}

type poolConfigParams struct {
	PoolID   int64          `json:"pool_id"`
	DeviceID string         `json:"device_id"`
	Spec     map[string]any `json:"spec"`
}

func decodePartitions(data map[string]any) []agent.PartitionInfo {
	return decodeInto[[]agent.PartitionInfo](data, agent.PartitionListDataKey)
}

func decodePartitionResult(data map[string]any) agent.PartitionResult {
	return decodeInto[agent.PartitionResult](data, agent.PartitionDataKey)
}

func decodePoolConfig(data map[string]any) agent.PoolConfigResult {
	return decodeInto[agent.PoolConfigResult](data, agent.PoolConfigDataKey)
}

func decodePoolUnmount(data map[string]any) agent.PoolUnmountResult {
	return decodeInto[agent.PoolUnmountResult](data, agent.PoolUnmountDataKey)
}

func decodeTrim(data map[string]any) agent.TrimResult {
	return decodeInto[agent.TrimResult](data, agent.TrimDataKey)
}

// decodeInto 从结果里取一个结构。
//
// 走一遍 JSON：同进程拿到的是结构体、跨进程是 map，两种形状都要能读；写
// 两遍解析逻辑迟早会漏掉其中一种。
func decodeInto[T any](data map[string]any, key string) T {
	var zero T
	if data == nil {
		return zero
	}
	raw, ok := data[key]
	if !ok {
		return zero
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return zero
	}
	var out T
	if err := json.Unmarshal(blob, &out); err != nil {
		log.Printf("[storage] 解析 %s 失败: %v", key, err)
		return zero
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Service) auditAction(
	ctx context.Context, operatorID int64, operatorName, clientIP, action string,
	nodeID, resourceID int64, success bool, detail string,
) {
	if s.audit == nil {
		return
	}
	params := map[string]any{}
	if detail != "" {
		params["detail"] = detail
	}
	s.record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "storage_pool", ResourceID: resourceID,
		Action: action, Params: params, Success: success, ClientIP: clientIP,
	})
}
