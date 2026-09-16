package vm

import (
	"context"
	"log"
	"strings"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// LockRequest 是加锁或解锁请求。
type LockRequest struct {
	Locked bool
	// Reason 是加锁原因（可选）。解锁时忽略。
	Reason string
}

// SetLock 加锁或解锁（F-2-12）。
//
// **同步生效、不入队、不下发**：锁只存在于控制面，虚拟化层根本不知道它的
// 存在，因此没有「需要重启才能生效」这回事——加锁之后立刻就能拦住删除。
//
// 加锁与解锁走同一个入口而不是两个接口：它们是同一个字段的两个取值，
// 分成两个接口就会出现「加锁用 POST /lock、解锁用 DELETE /lock」这种
// 不对称设计，而调用方迟早会搞混哪个是哪个。
//
// 解锁需要二次验证，但这一步在**接口层**完成（risk.Guard.Require），
// 不在服务层——判定口径集中在 internal/risk 的清单里（f-10-01 R-001），
// 服务层自行判断会让清单随时间失控。
func (s *Service) SetLock(
	ctx context.Context, vmID int64, req LockRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	// 维护模式的节点上不允许改锁：此时对节点的操作都会被拒绝，而能改锁
	// 会让用户误以为「还能操作」，点下去才发现别的都不行。
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	row, err := s.loadLock(ctx, vmID)
	if err != nil && !isNotFound(err) {
		return nil, err
	}
	if row == nil {
		row = &model.VMLock{VMID: vmID}
	}

	before := row.Locked
	now := time.Now()

	if req.Locked {
		if row.Locked {
			// 已经锁着就不再重复写：重复写会把 LockedAt 刷新成现在，
			// 让「锁了多久」这个信息失真——而排查时它往往是第一眼要看的东西。
			return s.viewWithLock(ctx, vm, row)
		}
		row.Locked = true
		row.LockedBy = &v.UserID
		row.Reason = optStr(strings.TrimSpace(req.Reason))
		row.LockedAt = &now
	} else {
		if !row.Locked {
			return s.viewWithLock(ctx, vm, row)
		}
		// 解锁时把三个字段一起清掉，而不是只翻 Locked。
		//
		// 只翻标志会留下「未锁定，但原因是『生产环境禁止删除』」这种自相
		// 矛盾的记录，界面要么显示一个不存在的理由，要么得写一层额外的
		// 判断去忽略它——两种都是在为省一次写入付代价。
		row.Locked = false
		row.LockedBy = nil
		row.Reason = nil
		row.LockedAt = nil
	}

	// 表上有 uniq_vm_lock_vm_id，但记录未必已存在（首次加锁），因此用
	// Save 的语义：有主键时更新，无主键时插入。
	if err := s.db.WithContext(ctx).Save(row).Error; err != nil {
		if isDuplicateKey(err) {
			// 并发加锁时另一侧刚插入。重读一次按更新处理——
			// 两个请求的意图一致，没有理由让其中一个失败。
			return s.SetLock(ctx, vmID, req, v, operatorName, clientIP)
		}
		log.Printf("[vm] 保存锁定状态失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	action := "vm.lock.apply"
	if !req.Locked {
		action = "vm.lock.release"
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:      action,
		Params:      map[string]any{"locked": req.Locked, "reason": derefStr(row.Reason)},
		BeforeState: map[string]any{"locked": before},
		AfterState:  map[string]any{"locked": row.Locked},
		Success:     true, ClientIP: clientIP,
	})

	return s.viewWithLock(ctx, vm, row)
}

// LockOf 返回虚拟机的锁定记录；未加锁时返回 nil。
//
// 导出供其他能力判断「这台机器能不能删」——但它们不该各自写一遍判断，
// 删除类入口应当在服务层统一调用 ensureNotLocked。
func (s *Service) LockOf(ctx context.Context, vmID int64) (*model.VMLock, error) {
	row, err := s.loadLock(ctx, vmID)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// ensureNotLocked 在删除类操作前检查锁。
//
// 文案要说明**怎么办**而不只是「不能删」：用户看到「已锁定」时的下一个
// 问题一定是「那我要做什么」，而答案（先解锁）不该让他自己去猜。
func (s *Service) ensureNotLocked(ctx context.Context, vm *model.VM) error {
	lock, err := s.LockOf(ctx, vm.ID)
	if err != nil {
		return err
	}
	if !lock.IsLocked() {
		return nil
	}

	msg := "该虚拟机已锁定，需先解锁才能删除"
	if reason := derefStr(lock.Reason); reason != "" {
		msg += "（锁定原因：" + reason + "）"
	}
	return api.ValidationFailed(msg)
}

// loadLock 读取锁定记录，不存在时返回 gorm.ErrRecordNotFound。
func (s *Service) loadLock(ctx context.Context, vmID int64) (*model.VMLock, error) {
	var row model.VMLock
	if err := s.db.WithContext(ctx).Where("vm_id = ?", vmID).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// lockMap 批量读取多台虚拟机的锁定状态。
//
// 一次查完而不是在循环里逐个查：列表页可能有上百台，逐个查会把一次列表
// 请求变成上百次数据库往返——而这恰恰是列表页最容易被察觉的卡顿来源。
func (s *Service) lockMap(
	ctx context.Context, vmIDs []int64,
) (map[int64]*model.VMLock, error) {
	out := make(map[int64]*model.VMLock, len(vmIDs))
	if len(vmIDs) == 0 {
		return out, nil
	}

	var rows []model.VMLock
	if err := s.db.WithContext(ctx).
		Where("vm_id IN ? AND locked = ?", vmIDs, true).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].VMID] = &rows[i]
	}
	return out, nil
}

// viewWithLock 构造带锁定状态的视图。
func (s *Service) viewWithLock(
	ctx context.Context, vm *model.VM, lock *model.VMLock,
) (*View, error) {
	view := toView(vm, lock, time.Now(), s.staleThreshold())
	return &view, nil
}
