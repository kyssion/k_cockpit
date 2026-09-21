package vm

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// auditEntryAssignOwner 构造归属变更的审计条目。
//
// before / after 都记下来：只记"现在属于谁"会丢掉"它原来属于谁"，而事后
// 最常被问到的正是那一个。
func auditEntryAssignOwner(
	v authz.Viewer, operatorName, clientIP string,
	vm *model.VM, before *int64, user model.User,
) audit.Entry {
	params := map[string]any{"to_user_id": user.ID, "to_username": user.Username}
	if before != nil {
		params["from_user_id"] = *before
	}
	return audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		Action:       "vm.assign_owner",
		Params:       params,
		Success:      true,
		ClientIP:     clientIP,
	}
}

// checkBandwidthQuota 校验该用户在这台节点上的**网卡限速合计**是否超出上限。
//
// 带宽是**速率型**配额：它没有"用满"的时刻，因此不走按月累计的
// resource_quota。这里的口径是"你同时最多能占多少带宽"——把各网卡的限速
// 加起来，而不是去统计历史流量。
//
// excludeNIC 是当前正在修改的那块网卡（改限速时要把它自己排除掉，否则
// 永远会算出"超限"）。
func (s *Service) checkBandwidthQuota(
	ctx context.Context, vm *model.VM, addMbps int, excludeNIC int64,
) error {
	if addMbps <= 0 || vm.OwnerID == nil {
		return nil
	}

	var user model.User
	if err := s.db.WithContext(ctx).Select("id", "max_bandwidth_mbps").
		Where("id = ?", *vm.OwnerID).First(&user).Error; err != nil {
		// 查不到就跳过：配额是可选能力，为一次统计失败拒绝加网卡代价不对等。
		log.Printf("[vm] 查询用户带宽上限失败 user=%d: %v", *vm.OwnerID, err)
		return nil
	}
	if user.MaxBandwidthMbps <= 0 {
		return nil // 不限
	}

	// 该用户在这台节点上的全部网卡限速之和。
	//
	// 按**节点**而不是全局统计：带宽是宿主机上行链路的资源，同一用户在两个
	// 节点上各占 100 Mbps 并不冲突。
	var used int64
	err := s.db.WithContext(ctx).Model(&model.VMInterface{}).
		Joins("JOIN vm ON vm.id = vm_interface.vm_id").
		Where("vm.node_id = ? AND vm.owner_id = ? AND vm.present = ?", vm.NodeID, *vm.OwnerID, true).
		Where("vm_interface.id <> ?", excludeNIC).
		Select("COALESCE(SUM(vm_interface.rate_limit_mbps), 0)").Scan(&used).Error
	if err != nil {
		log.Printf("[vm] 统计带宽占用失败: %v", err)
		return api.Internal()
	}
	if used+int64(addMbps) > int64(user.MaxBandwidthMbps) {
		return api.ValidationFailed(
			"带宽已达上限：" + itoa(used) + " + " + itoa(int64(addMbps)) + " Mbps 超过该用户的 " +
				itoa(int64(user.MaxBandwidthMbps)) + " Mbps")
	}
	return nil
}

// --- 归属分配 ---

// AssignOwner 把一台虚拟机指派给某个用户（F-1-07 的"分配已有 VM"）。
//
// 它存在的直接原因：管理员删除用户时，如果该用户名下还有虚拟机，目前只能
// 拒绝删除并提示"请先转移"——而"转移"这个动作从来没有实现过。
//
// 只有管理员能做：归属决定了谁能看见并操作这台机器，把它开放给普通用户
// 等于允许互相赠送（乃至抢占）机器。
func (s *Service) AssignOwner(
	ctx context.Context, vmID, userID int64, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	if !v.IsAdmin {
		return nil, api.PermissionDenied("仅管理员可分配虚拟机归属")
	}
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	// 已移入回收站的机器不再有归属可言：先恢复它，再谈分配给谁。
	if vm.DeletedAt != nil {
		return nil, api.NotFound("虚拟机不存在")
	}

	var user model.User
	if err := s.db.WithContext(ctx).Select("id", "username", "status").
		Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, api.NotFound("目标用户不存在")
	}
	// 封禁中的用户不该再拿到机器：那会让"封禁"只生效在界面上，而机器还在
	// 他名下继续运行。
	if user.Status == model.UserStatusBanned {
		return nil, api.ValidationFailed("该用户已被封禁，不能分配虚拟机给它")
	}

	before := vm.OwnerID
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vm.ID).
		Updates(map[string]any{"owner_id": userID, "updated_at": time.Now()}).Error; err != nil {
		log.Printf("[vm] 更新归属失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}
	vm.OwnerID = &userID

	s.record(ctx, auditEntryAssignOwner(v, operatorName, clientIP, vm, before, user))

	lock, err := s.LockOf(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	view := toView(vm, lock, time.Now(), s.staleThreshold())
	return &view, nil
}
