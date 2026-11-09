package vm

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/task"
)

// redefineDomain 让节点按回收站留存的域 XML 重建域（vm.restore）。
//
// 这是"恢复"与"重新创建"的分界线：重建用的是删除当时的域定义，因此
// 网卡 MAC、固件、机型、直通等配置都原样回来，不需要用户再填一遍。
func (s *Service) redefineDomain(ctx context.Context, vm *model.VM) error {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMRestore,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil {
		return api.Unavailable("节点不可达，恢复未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	return nil
}

// TrashItem 是回收站里的一台虚拟机。
type TrashItem struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	NodeID   int64  `json:"node_id"`
	VCPU     int    `json:"vcpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	Status   string `json:"status"`
	// DeletedAt 是移入回收站的时刻。
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Purgeable 为 false 表示这台机器**还不能**彻底删除（有在途任务）。
	Purgeable bool `json:"purgeable"`
}

// Trash 列出回收站里的虚拟机。
//
// 回收站不是一份独立的表，而是 **present = false 的那些记录**——删除本来就
// 保留记录（审计与历史任务都引用它）。把它们显式呈现出来，比再建一张表
// 更安全：不存在"两张表对不上"的可能。
func (s *Service) Trash(ctx context.Context, v authz.Viewer) ([]TrashItem, error) {
	query := s.db.WithContext(ctx).Model(&model.VM{}).Where("present = ?", false)
	if !v.IsAdmin {
		query = query.Where("owner_id = ?", v.UserID)
	}
	var rows []model.VM
	if err := query.Order("deleted_at DESC").Find(&rows).Error; err != nil {
		log.Printf("[vm] 查询回收站失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]TrashItem, 0, len(rows))
	for i := range rows {
		vm := &rows[i]
		active, err := s.hasActiveTask(ctx, vm.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, TrashItem{
			ID: vm.ID, Name: vm.Name, NodeID: vm.NodeID,
			VCPU: vm.VCPU, MemoryMB: vm.MemoryMB, DiskGB: vm.DiskGB,
			Status: vm.Status, DeletedAt: vm.DeletedAt,
			Purgeable: !active,
		})
	}
	return out, nil
}

// Restore 把回收站里的虚拟机放回列表。
//
// 恢复**不自动开机**：把它变回"可见"就够了，要不要开机由用户决定——自动
// 开机意味着一台被误删后恢复的机器会立刻开始占用资源并接收流量，而那
// 往往不是用户想要的。
func (s *Service) Restore(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	var vm model.VM
	err := s.db.WithContext(ctx).Where("id = ? AND present = ?", id, false).First(&vm).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("回收站里没有这台虚拟机")
	case err != nil:
		log.Printf("[vm] 查询回收站条目失败 id=%d: %v", id, err)
		return api.Internal()
	}
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return api.NotFound("回收站里没有这台虚拟机")
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return err
	}

	// 同名检查必须在恢复时做：删除之后用户很可能又建了一台同名的机器，
	// 而那时恢复会撞上唯一索引——报错来自数据库，用户看不懂。
	var dup int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND name = ? AND present = ?", vm.NodeID, vm.Name, true).
		Count(&dup).Error; err != nil {
		log.Printf("[vm] 检查同名失败: %v", err)
		return api.Internal()
	}
	if dup > 0 {
		return api.Conflict(
			"该节点上已经有一台同名且在线的「" + vm.Name + "」，请先改名或彻底删除其中一台")
	}

	// 恢复前确认域**还在节点上**。
	//
	// 移入回收站的任务会删除域定义（只保留磁盘），因此"恢复记录"并不等于
	// "机器回来了"。节点侧在移入回收站时留了一份域 XML，所以这里可以
	// **按那份 XML 重建域**——磁盘还在原处，重建出来就是原来那台机器。
	//
	// 只有在节点侧没有留存时（旧版本删除的记录、或删除方式为 transfer）
	// 才如实拒绝：宁可明确说"恢复不了"，也不给一台状态永远 unknown、
	// 开机必然失败的幽灵机器（实机抓出）。
	status, err := s.probeStatus(ctx, &vm)
	if err != nil {
		// 节点不可达是另一回事：不能说成"域不存在"，那会把一次网络问题
		// 指成一次数据丢失。原样上抛，由调用方看到 503。
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == api.CodeServiceUnavailable {
			return err
		}
		status = model.VMStatusUnknown
	}
	if status == model.VMStatusUnknown {
		if err := s.redefineDomain(ctx, &vm); err != nil {
			return err
		}
		// 重建后重新探测：恢复后的第一眼就该是真状态。
		status, err = s.probeStatus(ctx, &vm)
		if err != nil || status == model.VMStatusUnknown {
			return api.ValidationFailed(
				"已在节点上重建域定义，但无法确认其状态，请稍后在详情页刷新；" +
					"若仍显示未知，可彻底删除后重建")
		}
	}

	if err := s.db.WithContext(ctx).Model(&model.VM{}).Where("id = ?", id).
		Updates(map[string]any{
			"present": true, "deleted_at": nil,
			// 顺手把运行态校正成真实值：恢复后的第一眼就该是真状态。
			"status": status, "last_synced_at": time.Now(),
		}).Error; err != nil {
		log.Printf("[vm] 恢复虚拟机失败 id=%d: %v", id, err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.restore", Success: true, ClientIP: clientIP,
	})
	return nil
}

// Purge 彻底删除：下发节点删盘，成功后**物理删除**记录。
//
// 它与「移入回收站」是两步而不是一步：删盘不可逆，而"从列表里消失"可逆。
// 把不可逆的那一步单独放在回收站里，用户才有反悔的余地。
func (s *Service) Purge(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	var vm model.VM
	err := s.db.WithContext(ctx).Where("id = ? AND present = ?", id, false).First(&vm).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("回收站里没有这台虚拟机")
	case err != nil:
		log.Printf("[vm] 查询回收站条目失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return nil, api.NotFound("回收站里没有这台虚拟机")
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机还有任务在执行，请等待完成后再彻底删除")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDelete,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      derefOwner(vm.OwnerID),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id": vm.ID, "vm_name": vm.Name,
			"disk_action": DiskActionDelete,
			"purge":       true,
		},
		IdempotencyKey: "vm.purge:" + strconv.FormatInt(vm.ID, 10),
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action:  "vm.purge",
		Params:  map[string]any{"task_id": t.ID},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}
