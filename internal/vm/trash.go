package vm

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

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

	if err := s.db.WithContext(ctx).Model(&model.VM{}).Where("id = ?", id).
		Updates(map[string]any{"present": true, "deleted_at": nil}).Error; err != nil {
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
