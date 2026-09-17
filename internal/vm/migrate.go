package vm

import (
	"context"
	"errors"
	"fmt"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// MigrateRequest 是一次迁移请求。
type MigrateRequest struct {
	// ToNodeID 是目标节点。
	ToNodeID int64
}

// MigrationView 是一次迁移的对外视图。
type MigrationView struct {
	ID         int64  `json:"id"`
	VMID       int64  `json:"vm_id"`
	VMName     string `json:"vm_name"`
	FromNodeID int64  `json:"from_node_id"`
	ToNodeID   int64  `json:"to_node_id"`
	Status     string `json:"status"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`

	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// Migrate 受理一次跨节点迁移（F-2-09）。
//
// **这个方法的绝大部分是受理前的检查**，而那是有意的：迁移是本项目里
// 「前置条件最多」的操作，而每一个条件如果漏掉，代价都不是「操作失败」，
// 而是**两台宿主机上各留下一份不完整的东西**：
//
//   - 虚拟机没关机 → 磁盘在被写入的同时被复制，两边都不可用；
//   - 目标节点在维护 → 迁过去之后这台机器什么都做不了；
//   - 静态地址在目标节点上已被占用 → 迁过去之后两台机器抢同一个 IP；
//   - 端口转发冲突 → 宿主机上两条规则指向同一个端口。
//
// 这些都能在受理时查出来，而排进队列等执行到才发现的话，用户会在几分钟后
// 收到一条已经忘了背景的通知。
func (s *Service) Migrate(
	ctx context.Context, id int64, req MigrateRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	if req.ToNodeID <= 0 {
		return nil, api.InvalidParameter("必须指定目标节点")
	}
	if req.ToNodeID == target.NodeID {
		return nil, api.ValidationFailed("目标节点与当前节点相同，无需迁移")
	}

	if err := s.ensureMigrateTarget(ctx, req.ToNodeID); err != nil {
		return nil, err
	}

	// 实时探测，不用投影（f-2-01 R-002）。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		return nil, err
	}
	if current != model.VMStatusStopped {
		return nil, api.ValidationFailed(
			"迁移需要先关机（当前：" + DescribeStatus(current) + "）。" +
				"运行中迁移会让磁盘在被写入的同时被复制，两侧都不可用")
	}

	active, err := s.hasActiveTask(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	// 同一台机器同时只允许一次迁移。
	var running int64
	if err := s.db.WithContext(ctx).Model(&model.VMMigration{}).
		Where("vm_id = ? AND status IN ?", target.ID,
			[]string{model.MigrationPending, model.MigrationRunning}).
		Count(&running).Error; err != nil {
		log.Printf("[vm] 统计进行中的迁移失败: %v", err)
		return nil, api.Internal()
	}
	if running > 0 {
		return nil, api.Conflict("该虚拟机已有正在进行中的迁移")
	}

	// 节点内资源的冲突检查。这些是迁移**独有**的一类前置条件：
	// 静态地址与端口转发在各自节点内唯一，换一台宿主机就可能撞上别人。
	conflicts, err := s.migrationConflicts(ctx, target, req.ToNodeID)
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 {
		return nil, api.Conflict("目标节点上存在冲突，无法迁移：" +
			joinConflicts(conflicts) +
			"。这些资源在节点内唯一，请先在目标节点上释放它们，或先在源节点上解绑")
	}

	row := model.VMMigration{
		VMID: target.ID, VMName: target.Name,
		FromNodeID: target.NodeID, ToNodeID: req.ToNodeID,
		Status: model.MigrationPending, CreatedBy: &v.UserID,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[vm] 创建迁移记录失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type: model.TaskVMMigrate,
		// 资源锁键用**源节点**：迁移期间源侧不能被其它操作碰到。
		// 目标侧的资源锁由执行器自行向节点确认，不在这里占——
		// 一个任务只能有一个资源锁，而迁移天然横跨两台机器。
		NodeID:       target.NodeID,
		ResourceType: "vm",
		ResourceID:   target.ID,
		ResourceName: target.Name,
		OwnerID:      ownerOf(target, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"migration_id":    row.ID,
			"vm_id":           target.ID,
			"vm_name":         target.Name,
			"from_node_id":    target.NodeID,
			"to_node_id":      req.ToNodeID,
			"observed_status": current,
		},
	})
	if err != nil {
		if delErr := s.db.WithContext(ctx).Delete(&model.VMMigration{}, row.ID).Error; delErr != nil {
			log.Printf("[vm] 回滚迁移记录失败 id=%d: %v", row.ID, delErr)
		}
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: target.NodeID, ResourceType: "vm", ResourceID: target.ID,
		ResourceName: target.Name, Action: "vm.migrate",
		Params: map[string]any{
			"task_id": t.ID, "migration_id": row.ID,
			"from_node_id": target.NodeID, "to_node_id": req.ToNodeID,
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// ListMigrations 返回一台虚拟机的迁移记录。
func (s *Service) ListMigrations(
	ctx context.Context, id int64, v authz.Viewer,
) ([]MigrationView, error) {
	// 先做归属检查：直接查迁移表会绕过「这台虚拟机是不是你的」。
	if _, err := s.load(ctx, id, v); err != nil {
		return nil, err
	}

	var rows []model.VMMigration
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", id).Order("id DESC").Find(&rows).Error; err != nil {
		log.Printf("[vm] 查询迁移记录失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]MigrationView, 0, len(rows))
	for i := range rows {
		out = append(out, toMigrationView(&rows[i]))
	}
	return out, nil
}

// ensureMigrateTarget 校验目标节点。
//
// 维护模式在这里是**必须拦**的：迁过去之后这台机器什么都做不了（创建与
// 电源操作都被拒绝），而用户会以为是迁移本身出了问题。
func (s *Service) ensureMigrateTarget(ctx context.Context, nodeID int64) error {
	var n model.Node
	err := s.db.WithContext(ctx).
		Select("id", "maintenance_mode", "enroll_state").
		Where("id = ?", nodeID).First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.NotFound("目标节点不存在")
	case err != nil:
		log.Printf("[vm] 查询目标节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("目标节点尚未接入，无法作为迁移目标")
	}
	if n.MaintenanceMode {
		return api.ValidationFailed(
			"目标节点处于维护模式。迁过去之后这台机器将无法创建或开关机，" +
				"请先退出维护模式")
	}
	return nil
}

// migrationConflicts 检查节点内唯一资源在目标节点上的冲突。
//
// 返回的是**描述列表**而不是布尔值：用户需要知道具体是哪一项冲突。
// 只说「有冲突」等于让他自己去两个节点之间逐项对照——而这一页上就有
// 静态地址与端口转发两个列表。
func (s *Service) migrationConflicts(
	ctx context.Context, target *model.VM, toNodeID int64,
) ([]string, error) {
	var conflicts []string

	// 静态地址：`static_ip` 上有 uniq_static_ip_node_ip（节点内唯一）。
	var ips []model.StaticIP
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", target.ID).Find(&ips).Error; err != nil {
		log.Printf("[vm] 查询静态地址失败: %v", err)
		return nil, api.Internal()
	}
	for i := range ips {
		var count int64
		if err := s.db.WithContext(ctx).Model(&model.StaticIP{}).
			Where("node_id = ? AND ip = ?", toNodeID, ips[i].IP).
			Count(&count).Error; err != nil {
			log.Printf("[vm] 检查静态地址冲突失败: %v", err)
			return nil, api.Internal()
		}
		if count > 0 {
			conflicts = append(conflicts, fmt.Sprintf(
				"静态地址 %s 已在目标节点上被占用", ips[i].IP))
		}
	}

	// 端口转发：`(node_id, protocol, host_port)` 上唯一。
	var pfs []model.PortForward
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", target.ID).Find(&pfs).Error; err != nil {
		log.Printf("[vm] 查询端口转发失败: %v", err)
		return nil, api.Internal()
	}
	for i := range pfs {
		var count int64
		if err := s.db.WithContext(ctx).Model(&model.PortForward{}).
			Where("node_id = ? AND protocol = ? AND host_port = ?",
				toNodeID, pfs[i].Protocol, pfs[i].HostPort).
			Count(&count).Error; err != nil {
			log.Printf("[vm] 检查端口转发冲突失败: %v", err)
			return nil, api.Internal()
		}
		if count > 0 {
			conflicts = append(conflicts, fmt.Sprintf(
				"宿主机端口 %d/%s 已被目标节点上的其它转发占用",
				pfs[i].HostPort, pfs[i].Protocol))
		}
	}

	return conflicts, nil
}

// joinConflicts 把冲突列表拼成一句可读的话。
func joinConflicts(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += "；"
		}
		out += item
	}
	return out
}

func toMigrationView(row *model.VMMigration) MigrationView {
	view := MigrationView{
		ID: row.ID, VMID: row.VMID, VMName: row.VMName,
		FromNodeID: row.FromNodeID, ToNodeID: row.ToNodeID,
		Status:    row.Status,
		CreatedAt: row.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if row.Result != nil {
		view.Result = *row.Result
	}
	if row.Error != nil {
		view.Error = *row.Error
	}
	if row.FinishedAt != nil {
		view.FinishedAt = row.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}
