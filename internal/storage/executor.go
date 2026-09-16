package storage

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// createParams 是 storage.pool.create 任务的参数。
type createParams struct {
	NodeID     int64  `json:"node_id"`
	DeviceID   string `json:"device_id"`
	DevicePath string `json:"device_path"`
	FSType     string `json:"fs_type"`
	IsDefault  bool   `json:"is_default"`
	// HadData 记录受理时该设备是否已有数据，供事后追溯「这次格式化是否
	// 覆盖了原有内容」——用户日后发现数据丢了，这是第一个要查的信息。
	HadData bool `json:"had_data"`
}

// CreateExecutor 执行 storage.pool.create 任务。
type CreateExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewCreateExecutor 构造创建执行器。
func NewCreateExecutor(db *gorm.DB, client agent.Client) *CreateExecutor {
	return &CreateExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *CreateExecutor) Type() string { return model.TaskStoragePoolCreate }

// Run 下发格式化与挂载指令，成功后写入存储池记录。
//
// 顺序与前缀一致：**先让宿主机上的操作成功，再写控制面记录**。反过来的话，
// 格式化失败时会留下一条「存在但不可用」的池记录，用户看到它、拿它去建
// 虚拟机、然后在某个说不清的时刻失败。
func (e *CreateExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p createParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[storage] 解析创建参数失败: %v", err)
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpStoragePoolCreate,
		NodeID: p.NodeID,
		Target: p.DevicePath,
		Params: map[string]any{
			"device_id": p.DeviceID,
			"fs_type":   p.FSType,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，格式化指令未送达")
	}
	if !result.Success {
		// agent 侧的校验失败（系统盘、已挂载、被 LVM 占用等）在这里返回。
		// 它的信息比控制面校验更准确——控制面看到的可能是几分钟前的状态。
		return api.ValidationFailed(result.Message)
	}

	now := time.Now()
	pool := model.StoragePool{
		NodeID:         p.NodeID,
		DeviceID:       p.DeviceID,
		DevicePath:     &p.DevicePath,
		Kind:           "local",
		FSType:         &p.FSType,
		Status:         model.StoragePoolReady,
		IsDefault:      p.IsDefault,
		LastReportedAt: &now,
	}
	if mountPath, ok := result.Data["mount_path"].(string); ok && mountPath != "" {
		pool.MountPath = &mountPath
	}
	if status, ok := result.Data[agent.StatusDataKey].(string); ok && status != "" {
		pool.Status = status
	}

	if err := e.db.WithContext(ctx).Create(&pool).Error; err != nil {
		if !isDuplicateKey(err) {
			log.Printf("[storage] 写入存储池记录失败: %v", err)
			return api.Internal()
		}

		// 到这里有两种可能：设备已被占用，或本节点已有默认池。
		//
		// 判定方式是用**再试一次非默认**去区分，而不是匹配错误信息里的索引名。
		// 那个名字依赖驱动：PostgreSQL 会带（duplicate key ... "uniq_storage_pool_default"），
		// SQLite 只给 "UNIQUE constraint failed: storage_pool.node_id"——
		// 靠它判定会让退让逻辑在 SQLite 上静默失效，而测试库正是 SQLite，
		// 于是这条分支永远没被验证过。
		//
		// 默认池冲突需要退让：宿主机上的池已经建好了，此时报失败会让用户
		// 以为白干一场。而设备占用**不能**退让——退了也还是占着，必须报错。
		if p.IsDefault {
			pool.IsDefault = false
			if retryErr := e.db.WithContext(ctx).Create(&pool).Error; retryErr == nil {
				log.Printf("[storage] 本节点已有默认池，%s 已创建为非默认池 id=%d",
					p.DevicePath, pool.ID)
				return nil
			}
		}
		// 非默认仍然冲突，说明是设备被占用——这是用户需要知道的事，
		// 且换成非默认池也解决不了。
		return api.Conflict("该设备已被存储池占用")
	}

	log.Printf("[storage] 已创建存储池 id=%d node=%d device=%s task=%d",
		pool.ID, pool.NodeID, pool.DeviceID, t.ID)
	return nil
}

// deleteParams 是 storage.pool.delete 任务的参数。
type deleteParams struct {
	PoolID     int64  `json:"pool_id"`
	NodeID     int64  `json:"node_id"`
	DeviceID   string `json:"device_id"`
	DevicePath string `json:"device_path"`
}

// DeleteExecutor 执行 storage.pool.delete 任务。
type DeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDeleteExecutor 构造删除执行器。
func NewDeleteExecutor(db *gorm.DB, client agent.Client) *DeleteExecutor {
	return &DeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DeleteExecutor) Type() string { return model.TaskStoragePoolDelete }

// Run 下发卸载与删除指令，成功后软删除记录。
func (e *DeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p deleteParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[storage] 解析删除参数失败: %v", err)
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpStoragePoolDelete,
		NodeID: p.NodeID,
		Target: p.DevicePath,
		Params: map[string]any{"device_id": p.DeviceID},
	})
	if err != nil {
		return api.Unavailable("节点不可达，删除指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 软删除并**清掉默认标记**：留下一个指向已删除设备的默认池，会让
	// 「该节点有默认池」与「默认池已不存在」同时成立，创建虚拟机时会
	// 定位到一个消失的目标。
	now := time.Now()
	err = e.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("id = ?", p.PoolID).
		Updates(map[string]any{
			"deleted_at": now,
			"is_default": false,
		}).Error
	if err != nil {
		log.Printf("[storage] 标记存储池已删除失败 id=%d: %v", p.PoolID, err)
	}

	log.Printf("[storage] 已删除存储池 id=%d device=%s task=%d", p.PoolID, p.DevicePath, t.ID)
	return nil
}

// isDuplicateKey 判断错误是否为唯一约束冲突。
func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

// 这里曾有一个 isDefaultConflict，靠「错误信息里是否含索引名
// uniq_storage_pool_default」来区分默认池冲突与设备占用。
//
// 它已删除：索引名并非各驱动都会返回（SQLite 只给列名），因此那条退让分支
// 在测试库上从不生效，也就从没被验证过。现在改为**按行为区分**——再试一次
// 非默认池，能成就是默认池冲突，还冲突就是设备被占用。判断依据从「驱动
// 怎么措辞」换成了「数据库实际允许什么」，这是唯一跨驱动都成立的依据。
