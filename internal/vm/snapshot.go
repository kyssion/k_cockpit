package vm

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// defaultSnapshotQuota 是虚拟机未指定模板配额时的快照上限。
//
// 刻意**不**做成设置项：配额最终应来自模板（`template.max_snapshots`），
// 而模板功能尚未实现。先放一个临时设置项，会在模板上线后留下两个来源，
// 而「哪一个是生效的」正是最难排查的一类问题。
const defaultSnapshotQuota = 10

// SnapshotView 是快照在接口层的形态。
type SnapshotView struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Description   *string `json:"description,omitempty"`
	Kind          string  `json:"kind"`
	IncludeMemory bool    `json:"include_memory"`
	// VMStatus 是创建快照时虚拟机的状态。
	VMStatus  *string   `json:"vm_status,omitempty"`
	SizeBytes int64     `json:"size_bytes"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`

	HasChildren bool `json:"has_children"`
	IsCurrent   bool `json:"is_current"`

	// CanDelete / CanRestore 由**后端**算好，界面据此禁用按钮。
	//
	// 不让前端按 has_children / is_current 自己拼判断：那样「哪些条件下能
	// 删」这条规则会存在两份，而界面上的那份迟早与后端不一致——表现为
	// 按钮可点、但点下去必然失败。
	CanDelete  bool `json:"can_delete"`
	CanRestore bool `json:"can_restore"`
}

// SnapshotList 是快照列表与配额。
type SnapshotList struct {
	Items []SnapshotView `json:"items"`
	// Quota 是上限，Used 是已用数量。两者一起返回，界面才能显示「已用/上限」
	// 而不必自己再查一次配额。
	Quota int `json:"quota"`
	Used  int `json:"used"`
}

// Snapshots 返回虚拟机的快照列表。
func (s *Service) Snapshots(
	ctx context.Context, vmID int64, v authz.Viewer,
) (*SnapshotList, error) {
	if _, err := s.load(ctx, vmID, v); err != nil {
		return nil, err
	}

	var rows []model.VMSnapshot
	err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).
		Order("created_at DESC, id DESC").
		Find(&rows).Error
	if err != nil {
		log.Printf("[vm] 查询快照失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	items := make([]SnapshotView, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		items = append(items, SnapshotView{
			ID: r.ID, Name: r.Name, Description: r.Description,
			Kind: r.Kind, IncludeMemory: r.IncludeMemory,
			VMStatus: r.VMStatus, SizeBytes: r.SizeBytes,
			Status: r.Status, CreatedAt: r.CreatedAt,
			HasChildren: r.HasChildren, IsCurrent: r.IsCurrent,
			CanDelete: r.CanDelete(),
			// 只有 ready 能恢复：creating 时快照还没写完，error 时它不可信；
			// 已经运行在这个快照上（is_current）时恢复更没有意义。
			CanRestore: r.IsReady() && !r.IsCurrent,
		})
	}

	return &SnapshotList{Items: items, Quota: defaultSnapshotQuota, Used: len(items)}, nil
}

// CreateSnapshotRequest 是一次创建快照的请求。
type CreateSnapshotRequest struct {
	Name          string
	Description   string
	IncludeMemory bool
}

// CreateSnapshot 受理一次创建快照。
//
// 与创建虚拟机**相反**，这里先写记录再入队：执行器需要知道快照的 ID 才能
// 把结果写回同一条记录。创建虚拟机时可以先执行再写投影（因为投影只是缓存），
// 而快照这一行在创建过程中就承担了「进行中的任务」这一职责。
//
// 代价是失败时可能留下一条 status=creating 的死记录——执行器把它标成 error，
// 用户可以在界面上删掉它。
func (s *Service) CreateSnapshot(
	ctx context.Context, vmID int64, req CreateSnapshotRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, api.InvalidParameter("请填写快照名称")
	}

	// 配额在**创建之前**检查，而不是先创建再发现超限——
	// 那时已经产生了一个需要回滚的快照，而回滚本身也可能失败。
	var used int64
	if err := s.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("vm_id = ?", vmID).Count(&used).Error; err != nil {
		log.Printf("[vm] 统计快照数量失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}
	if int(used) >= defaultSnapshotQuota {
		return nil, api.Conflict(
			"快照数量已达上限（" + strconv.Itoa(defaultSnapshotQuota) + "），请先删除不再需要的快照")
	}

	// 实时探测运行态：投影不参与业务判定（f-2-01 R-002），而这里需要它来
	// 决定用哪种快照方式，判断错了会得到一个不可靠的快照。
	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	row := model.VMSnapshot{
		VMID: vm.ID, NodeID: vm.NodeID,
		Name:          name,
		Description:   optStr(strings.TrimSpace(req.Description)),
		IncludeMemory: req.IncludeMemory,
		VMStatus:      &current,
		Status:        model.SnapshotCreating,
		Kind:          snapshotKind(req.IncludeMemory, current),
		CreatedBy:     &v.UserID,
	}

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该虚拟机已有同名快照")
		}
		log.Printf("[vm] 创建快照记录失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	params := snapshotCreateParams{
		VMID: vm.ID, VMName: vm.Name,
		SnapshotID: row.ID, SnapshotName: row.Name,
		Kind: row.Kind, IncludeMemory: row.IncludeMemory,
		ObservedStatus: current,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMSnapshotCreate,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		// 入队失败时把记录标成 error 而不是删掉：留下痕迹能解释「为什么
		// 界面上没有这个快照」，直接删掉则会让用户的这次操作彻底无迹可循。
		s.markSnapshot(ctx, row.ID, model.SnapshotError)
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:      "vm.snapshot.create.request",
		Params:      params,
		BeforeState: map[string]any{"status": current},
		AfterState:  map[string]any{"snapshot_id": row.ID, "task_id": t.ID},
		Success:     true, ClientIP: clientIP,
	})
	return t, nil
}

// snapshotKind 推导快照种类（F-2-07 的「策略自动选择」）。
//
// internal / external 是虚拟化层的实现细节，用户关心的是「要不要保存运行
// 现场」。把推导放在后端，界面就只需要一个勾选框。
func snapshotKind(includeMemory bool, current string) string {
	// 含内存必须是内部快照：外部快照（--disk-only）不保存内存。
	if includeMemory {
		return model.SnapshotKindInternal
	}
	// 运行中且不含内存：用外部快照。
	//
	// 对运行中的磁盘做内部快照，需要 QEMU 在持续写入的情况下保证一致，
	// 而来宾的文件系统缓存此时是活跃的——得到的状态难以保证可靠。外部
	// 快照把当前状态切出来另存，反而更干净。
	if current == model.VMStatusRunning {
		return model.SnapshotKindExternal
	}
	// 关机态：内部快照更快，且没有一致性问题。
	return model.SnapshotKindInternal
}

// RestoreSnapshot 受理一次快照恢复。
func (s *Service) RestoreSnapshot(
	ctx context.Context, vmID, snapshotID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	snap, err := s.loadSnapshot(ctx, vmID, snapshotID)
	if err != nil {
		return nil, err
	}
	if !snap.IsReady() {
		return nil, api.ValidationFailed(
			"快照当前状态为「" + snapshotStatusLabel(snap.Status) + "」，不能用于恢复")
	}
	if snap.IsCurrent {
		return nil, api.ValidationFailed("虚拟机当前正运行在这个快照上，无需恢复")
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	// 在途任务存在时拒绝恢复，而不是排队。
	//
	// 与电源操作不同：恢复会**覆盖整个磁盘状态**，排在别的操作后面执行
	// 意味着「用户以为取消了的操作其实照样做了」——这一点与删除同理。
	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	params := snapshotRestoreParams{
		VMID: vm.ID, VMName: vm.Name,
		SnapshotID: snap.ID, SnapshotName: snap.Name,
		DomainName:     derefStr(snap.DomainName),
		ObservedStatus: current,
		// 创建快照时的状态一并带上：执行器据此决定是否要先关机。
		SnapshotVMStatus: derefStr(snap.VMStatus),
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMSnapshotRestore,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.markSnapshot(ctx, snap.ID, model.SnapshotRestoring)

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:      "vm.snapshot.restore.request",
		Params:      params,
		BeforeState: map[string]any{"status": current, "snapshot": snap.Name},
		AfterState:  map[string]any{"task_id": t.ID},
		// 恢复会**丢弃**快照之后的所有磁盘改动，属于不可逆操作（f-10-02
		// 的判定口径）。这里记为 true 是因为「请求已受理」，真正的执行
		// 结果由任务队列写审计。
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// DeleteSnapshot 受理一次快照删除。
func (s *Service) DeleteSnapshot(
	ctx context.Context, vmID, snapshotID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	snap, err := s.loadSnapshot(ctx, vmID, snapshotID)
	if err != nil {
		return nil, err
	}

	// 拒绝理由要**具体**，而不是笼统的「不能删除」：
	// 「有子快照」与「是当前快照」需要用户做的事完全不同。
	switch {
	case snap.HasChildren:
		return nil, api.ValidationFailed("该快照存在子快照，请先删除子快照")
	case snap.IsCurrent:
		return nil, api.ValidationFailed("虚拟机当前正运行在这个快照上，不能删除")
	case snap.Status == model.SnapshotCreating:
		return nil, api.Conflict("快照正在创建中，请等待完成")
	case snap.Status == model.SnapshotDeleting:
		return nil, api.Conflict("快照正在删除中")
	}

	params := snapshotDeleteParams{
		VMID: vm.ID, VMName: vm.Name,
		SnapshotID: snap.ID, SnapshotName: snap.Name,
		DomainName: derefStr(snap.DomainName),
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMSnapshotDelete,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.markSnapshot(ctx, snap.ID, model.SnapshotDeleting)

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:     "vm.snapshot.delete.request",
		Params:     params,
		AfterState: map[string]any{"task_id": t.ID},
		Success:    true, ClientIP: clientIP,
	})
	return t, nil
}

// loadSnapshot 读取属于该虚拟机的快照。
func (s *Service) loadSnapshot(
	ctx context.Context, vmID, snapshotID int64,
) (*model.VMSnapshot, error) {
	var row model.VMSnapshot
	err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", snapshotID, vmID).
		First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("快照不存在")
	case err != nil:
		log.Printf("[vm] 查询快照失败 id=%d: %v", snapshotID, err)
		return nil, api.Internal()
	}
	return &row, nil
}

// markSnapshot 更新快照状态。
//
// 失败时只记日志：这一步是给执行器留的路标，标不上不该让已经受理的任务失败。
func (s *Service) markSnapshot(ctx context.Context, id int64, status string) {
	err := s.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": time.Now()}).Error
	if err != nil {
		log.Printf("[vm] 更新快照状态失败 id=%d status=%s: %v", id, status, err)
	}
}

// snapshotStatusLabel 返回快照状态的中文名。
func snapshotStatusLabel(status string) string {
	switch status {
	case model.SnapshotCreating:
		return "创建中"
	case model.SnapshotReady:
		return "就绪"
	case model.SnapshotRestoring:
		return "恢复中"
	case model.SnapshotDeleting:
		return "删除中"
	case model.SnapshotError:
		return "失败"
	default:
		return status
	}
}

func derefStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
