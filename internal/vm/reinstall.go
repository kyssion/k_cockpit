package vm

import (
	"context"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// reinstallTemplate 取出并校验用于重装的模板。
//
// 校验口径与克隆一致（同节点、就绪、允许克隆、可见），因为它本质上就是
// 「拿这个模板的系统盘替换掉现在那块」——只是替换的目标是已有虚拟机而不是
// 新建一台。
func (s *Service) reinstallTemplate(
	ctx context.Context, id int64, v authz.Viewer,
) (*model.Template, error) {
	var tpl model.Template
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&tpl).Error
	if err != nil {
		return nil, api.NotFound("模板不存在")
	}
	if !v.IsAdmin && !tpl.Published && (tpl.CreatedBy == nil || *tpl.CreatedBy != v.UserID) {
		return nil, api.NotFound("模板不存在")
	}
	switch {
	case tpl.Status == model.TemplatePreparing:
		return nil, api.ValidationFailed("模板正在制备中，完成后才能用于重装")
	case tpl.Status != model.TemplateReady:
		return nil, api.ValidationFailed("模板制备失败，请删除后重新制备")
	case !tpl.CloneEnabled:
		return nil, api.ValidationFailed("该模板已停止提供克隆")
	}
	return &tpl, nil
}

// Reinstall 用模板重建虚拟机的系统盘（F-2-11）。
//
// 保留的是：**硬件配置、数据盘与主网口绑定**。被替换的只有系统盘。
//
// 这是本项目里对单台虚拟机破坏性最强的操作（比删除轻一档，但同样不可逆），
// 因此走二次验证（见 internal/risk 的清单）。
//
// 几点必须的前置条件，都在受理时同步判定：
//   - 关机：替换一块正在被写入的系统盘得不到可用的结果；
//   - 无在途任务：同一台机器只允许一个进行中的操作（f-2-11 明文要求）；
//   - 没有未清理的备份：第二次重装会覆盖上一次的备份，而用户可能正指望
//     那条「回到上一个系统」的退路。**显式拒绝而不是静默覆盖**。
func (s *Service) Reinstall(
	ctx context.Context, id, templateID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, target.NodeID); err != nil {
		return nil, err
	}

	// 未清理的备份会阻止第二次重装：静默覆盖等于悄悄拿走用户唯一的退路。
	if target.ReinstallBackup != nil && *target.ReinstallBackup != "" {
		return nil, api.Conflict(
			"存在上次重装留下的系统盘备份，请先在页面上清理它——" +
				"否则这次重装会覆盖掉你回到原系统的唯一退路")
	}

	// 实时探测，不用投影（f-2-01 R-002）。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		return nil, err
	}
	if current != model.VMStatusStopped {
		return nil, api.ValidationFailed(
			"重装需要先关机（当前：" + DescribeStatus(current) + "）")
	}

	// 同一台机器只允许一个进行中的操作（f-2-11）。
	active, err := s.hasActiveTask(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	tpl, err := s.reinstallTemplate(ctx, templateID, v)
	if err != nil {
		return nil, err
	}
	// 模板盘就在它所属节点的存储池里，跨节点使用需要先导出再导入。
	if tpl.NodeID != target.NodeID {
		return nil, api.ValidationFailed(
			"模板与虚拟机不在同一台宿主机上；跨节点使用需要先导出再导入")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMReinstall,
		NodeID:       target.NodeID,
		ResourceType: "vm",
		ResourceID:   target.ID,
		ResourceName: target.Name,
		OwnerID:      ownerOf(target, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id":              target.ID,
			"vm_name":            target.Name,
			"template_id":        tpl.ID,
			"template_disk_path": tpl.DiskPathOf(),
			"disk_gb":            max(tpl.MinDiskGB, target.DiskGB),
			"observed_status":    current,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: target.NodeID, ResourceType: "vm", ResourceID: target.ID, ResourceName: target.Name,
		Action:  "vm.reinstall",
		Params:  map[string]any{"task_id": t.ID, "template_id": tpl.ID, "template_name": tpl.Name},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// PurgeBackup 清理重装留下的备份盘（F-2-11）。
//
// 单独成一个操作而不是重装的一部分：备份在重装**成功之后**继续存在，
// 什么时候扔掉由用户决定。并进重装等于替用户决定「不再需要回到原来的
// 系统了」——而那份备份是他唯一的退路。
//
// **不需要二次验证**：这是一个纯删除操作，删掉的是已经不再被使用的备份，
// 虚拟机的当前运行不受任何影响。给它加验证只会稀释真正危险操作的份量。
func (s *Service) PurgeBackup(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if target.ReinstallBackup == nil || *target.ReinstallBackup == "" {
		return nil, api.ValidationFailed("该虚拟机没有需要清理的备份")
	}

	// 运行中不允许清理：备份盘此时可能正被当作底层链的一部分（真实实现
	// 里系统盘可能是从备份派生的 overlay），删掉它会让运行中的虚拟机
	// 在读到某块未缓存的数据时崩掉。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		return nil, err
	}
	if current != model.VMStatusStopped {
		return nil, api.ValidationFailed(
			"清理备份需要先关机（当前：" + DescribeStatus(current) + "）")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMReinstallPurge,
		NodeID:       target.NodeID,
		ResourceType: "vm",
		ResourceID:   target.ID,
		ResourceName: target.Name,
		OwnerID:      ownerOf(target, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id":       target.ID,
			"vm_name":     target.Name,
			"backup_path": *target.ReinstallBackup,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: target.NodeID, ResourceType: "vm", ResourceID: target.ID, ResourceName: target.Name,
		Action:  "vm.reinstall.purge",
		Params:  map[string]any{"task_id": t.ID},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}
