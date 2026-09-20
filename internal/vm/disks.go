package vm

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// DiskView 是一块磁盘的视图。
type DiskView struct {
	Dev          string `json:"dev"`
	CapacityGB   int    `json:"capacity_gb"`
	ActualBytes  int64  `json:"actual_bytes"`
	Format       string `json:"format"`
	Bus          string `json:"bus"`
	Source       string `json:"source"`
	IsSystem     bool   `json:"is_system"`
	Hotpluggable bool   `json:"hotpluggable"`

	// 下面三项由**控制面**按运行态算出，而不是节点给。
	//
	// 节点只报告事实（这块盘是什么、当前能不能热插拔）；"现在能不能卸载"
	// 是一个规则，规则只有一份才不会分叉——界面按它禁用按钮，后端按它拒绝
	// 请求。
	CanDetach bool `json:"can_detach"`
	// DetachReason 说明为什么不能卸载。空着的话用户只会看到一个灰按钮，
	// 而他会去猜是不是权限问题。
	DetachReason    string `json:"detach_reason,omitempty"`
	CanChangeBus    bool   `json:"can_change_bus"`
	ChangeBusReason string `json:"change_bus_reason,omitempty"`

	// CanMigrate 表示这块盘能否迁移到别的存储池。
	//
	// 系统盘**可以**迁移（它只是一块盘），运行中也能迁（热迁移由节点决定
	// 能不能做）。控制面唯一能确定的障碍是"没有别的目标池"，而那是列表级
	// 的事实，因此这里只在没有目标时给出理由。
	CanMigrate    bool   `json:"can_migrate"`
	MigrateReason string `json:"migrate_reason,omitempty"`
}

// DiskTarget 是一个可迁移到的目标存储池。
type DiskTarget struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Path     string  `json:"path,omitempty"`
	UsableGB float64 `json:"usable_gb"`
}

// AttachableDisk 是一个可以挂给虚拟机的虚拟磁盘文件。
type AttachableDisk struct {
	ID        int64  `json:"id"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
}

// DiskListView 是「磁盘」标签页的数据。
type DiskListView struct {
	// Status 是探测到的运行态。操作可用性以它为依据，而不是投影——
	// 投影可能滞后，按它禁用按钮会在机器实际已关机时仍显示"需关机"。
	Status string     `json:"status"`
	Disks  []DiskView `json:"disks"`
	// BusOptions 取自配置矩阵里 disk_bus 的可选值：换总线的下拉框与
	// 「创建时选的驱动」是同一个集合，不另写一份。
	BusOptions  []EditOption     `json:"bus_options"`
	Attachables []AttachableDisk `json:"attachables"`
	// MigrateTargets 是可迁移过去的存储池。为空时界面不显示迁移入口：
	// 一个点了必然失败的下拉框，比没有这个按钮更糟。
	MigrateTargets []DiskTarget `json:"migrate_targets"`
	// Limits 是当前生效的磁盘限速（IOPS 与吞吐），供磁盘标签页直接编辑——
	// 它们此前只在「编辑」标签里，而用户找限速时不会去那里找。
	Limits DiskLimits `json:"limits"`
}

// DiskLimits 是磁盘限速的当前值。
//
// IOPS 与吞吐**并存**而不是二选一：它们限制的是不同性质的负载（小块随机
// 读写先撞 IOPS，大块顺序读写先撞吞吐）。每组内部的「总量」与「读写分离」
// 互斥，由服务端校验。
type DiskLimits struct {
	IOPSTotal  int `json:"iops_total"`
	IOPSRead   int `json:"iops_read"`
	IOPSWrite  int `json:"iops_write"`
	BytesTotal int `json:"bytes_total"`
	BytesRead  int `json:"bytes_read"`
	BytesWrite int `json:"bytes_write"`
}

// DiskChangeRequest 是一次磁盘变更。
type DiskChangeRequest struct {
	// Action 取值 attach / detach / bus。
	Action string
	// Dev 是目标设备名；attach 时留空（由节点分配）。
	Dev string
	// Bus 仅在 action = bus 时有效。
	Bus string
	// FileID 仅在 action = attach 时有效，指向 storage_file（category=disk）。
	FileID int64
	// TargetPoolID 仅在 action = migrate 时有效，指向目标 storage_pool。
	TargetPoolID int64
	// AllowHot 表示运行中仍继续（热迁移）。它由用户在界面上确认，而不是
	// 服务端默认——热迁移期间磁盘仍在使用，是否接受那段抖动只有使用者知道。
	AllowHot bool
}

// Disks 读取虚拟机的磁盘列表。
//
// **实时探测而非读表**：磁盘是虚拟化层的状态，控制面不持有它们的记录。
// 存一份就要承担对账的责任，而多一块少一块都不会报错——只会在某次操作时
// 变成"改了一块不存在的盘"。
func (s *Service) Disks(ctx context.Context, vmID int64, v authz.Viewer) (*DiskListView, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	status, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	disks, err := s.probeDisks(ctx, vm)
	if err != nil {
		return nil, err
	}

	attachables, err := s.attachableDisks(ctx, vm.NodeID, v)
	if err != nil {
		return nil, err
	}
	targets, err := s.migrateTargets(ctx, vm.NodeID)
	if err != nil {
		return nil, err
	}

	running := status == model.VMStatusRunning
	out := &DiskListView{
		Status:         status,
		Disks:          make([]DiskView, 0, len(disks)),
		BusOptions:     busOptions(),
		Attachables:    attachables,
		MigrateTargets: targets,
		Limits: DiskLimits{
			IOPSTotal: vm.DiskIOPSTotal, IOPSRead: vm.DiskIOPSRead, IOPSWrite: vm.DiskIOPSWrite,
			BytesTotal: vm.DiskBytesTotal, BytesRead: vm.DiskBytesRead, BytesWrite: vm.DiskBytesWrite,
		},
	}
	for i := range disks {
		d := &disks[i]
		view := DiskView{
			Dev: d.Dev, CapacityGB: d.CapacityGB, ActualBytes: d.ActualBytes,
			Format: d.Format, Bus: d.Bus, Source: d.Source,
			IsSystem: d.IsSystem, Hotpluggable: d.Hotpluggable,
		}
		switch {
		case d.IsSystem:
			view.CanDetach = false
			view.DetachReason = "系统盘不可卸载"
		case running && !d.Hotpluggable:
			view.CanDetach = false
			view.DetachReason = "运行中且该设备不支持热插拔，需先关机"
		default:
			view.CanDetach = true
		}
		if running {
			view.CanChangeBus = false
			view.ChangeBusReason = "换总线需要关机：来宾里的设备路径会变，运行中改会导致盘符漂移"
		} else {
			view.CanChangeBus = true
		}
		if len(targets) == 0 {
			view.CanMigrate = false
			view.MigrateReason = "该节点上还没有第二个存储池，无处可迁"
		} else {
			view.CanMigrate = true
		}
		out.Disks = append(out.Disks, view)
	}
	return out, nil
}

// probeDisks 向节点查询磁盘列表。
func (s *Service) probeDisks(ctx context.Context, vm *model.VM) ([]agent.VMDisk, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMDiskList,
		NodeID: vm.NodeID,
		Target: vm.Name,
		Params: map[string]any{"vm_id": vm.ID, "vm_name": vm.Name},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，读不到磁盘列表")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	raw, ok := result.Data[agent.VMDiskListDataKey]
	if !ok {
		// 节点没给数据不算错误——它可能刚升级、还没实现这个操作。给一个空
		// 列表并让界面显示"节点未提供"，比报 500 更接近事实。
		return nil, nil
	}
	// 走一遍 JSON：同进程时拿到的是结构体，跨进程时是 map，两种形状都要
	// 能读出来，而分别处理会让其中一条路径永远没被走到。
	blob, err := json.Marshal(raw)
	if err != nil {
		log.Printf("[vm] 序列化磁盘列表失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}
	var disks []agent.VMDisk
	if err := json.Unmarshal(blob, &disks); err != nil {
		log.Printf("[vm] 解析磁盘列表失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}
	return disks, nil
}

// attachableDisks 列出可挂给虚拟机的虚拟磁盘文件（我的存储里的 disk 类）。
func (s *Service) attachableDisks(
	ctx context.Context, nodeID int64, v authz.Viewer,
) ([]AttachableDisk, error) {
	var files []model.StorageFile
	err := s.db.WithContext(ctx).
		Where(`node_id = ? AND category = ? AND uploaded_at IS NOT NULL
			AND (user_id IS NULL OR user_id = ?)`, nodeID, model.FileCategoryDisk, v.UserID).
		Order("filename").Find(&files).Error
	if err != nil {
		log.Printf("[vm] 查询可挂载磁盘失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	out := make([]AttachableDisk, 0, len(files))
	for i := range files {
		out = append(out, AttachableDisk{
			ID: files[i].ID, Filename: files[i].Filename, SizeBytes: files[i].SizeBytes,
		})
	}
	return out, nil
}

// migrateTargets 列出可作为迁移目标的存储池（该节点上、状态就绪的）。
//
// 只给"就绪"的池：往一个正在初始化或已故障的池里搬家，失败发生在搬了一半
// 的时候，而那时源可能已经被删了。
func (s *Service) migrateTargets(ctx context.Context, nodeID int64) ([]DiskTarget, error) {
	var pools []model.StoragePool
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND status = ?", nodeID, model.StoragePoolReady).
		Order("is_default DESC, id").Find(&pools).Error; err != nil {
		log.Printf("[vm] 查询存储池失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	out := make([]DiskTarget, 0, len(pools))
	for i := range pools {
		p := &pools[i]
		name := ""
		if p.Remark != nil {
			name = *p.Remark
		}
		if name == "" {
			name = poolLabel(p)
		}
		t := DiskTarget{ID: p.ID, Name: name, UsableGB: float64(p.UsableBytes) / (1 << 30)}
		if p.MountPath != nil {
			t.Path = *p.MountPath
		}
		out = append(out, t)
	}
	return out, nil
}

// poolLabel 给存储池一个可读的名字。
//
// 池本身没有 name 列（它是"一块设备挂在哪"这一事实），因此用设备路径兜底。
// 界面上显示 /dev/sdb 比显示"存储池 #3"有用得多——后者无法对上是哪块盘。
func poolLabel(p *model.StoragePool) string {
	if p.MountPath != nil && *p.MountPath != "" {
		return *p.MountPath
	}
	if p.DevicePath != nil && *p.DevicePath != "" {
		return *p.DevicePath
	}
	return "存储池 #" + strconv.FormatInt(p.ID, 10)
}

// busOptions 取磁盘驱动的可选值（与配置矩阵同源）。
func busOptions() []EditOption {
	for _, f := range editFields {
		if f.Key == "disk_bus" {
			return f.Options
		}
	}
	return nil
}

// ChangeDisk 受理一次磁盘变更并入队。
func (s *Service) ChangeDisk(
	ctx context.Context, vmID int64, req DiskChangeRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	switch req.Action {
	case agent.DiskActionAttach, agent.DiskActionDetach, agent.DiskActionBus, agent.DiskActionMigrate:
	default:
		return nil, api.InvalidParameter("不支持的磁盘操作，可选 attach / detach / bus / migrate")
	}

	status, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	running := status == model.VMStatusRunning

	params := map[string]any{
		"action":  req.Action,
		"vm_id":   vm.ID,
		"vm_name": vm.Name,
		"dev":     req.Dev,
	}

	switch req.Action {
	case agent.DiskActionAttach:
		if req.FileID <= 0 {
			return nil, api.InvalidParameter("请选择要挂载的磁盘文件")
		}
		file, err := s.loadAttachable(ctx, vm.NodeID, req.FileID, v)
		if err != nil {
			return nil, err
		}
		// 只给相对路径与大小：绝对路径由节点按该用户的存储根拼出来。
		// 控制面拼路径的话，存储根一换（换盘、迁移）库里的记录就全指错了。
		params["file_id"] = file.ID
		params["rel_path"] = file.RelPath
		params["size_bytes"] = file.SizeBytes
		params["filename"] = file.Filename

	case agent.DiskActionDetach:
		if req.Dev == "" {
			return nil, api.InvalidParameter("请指定要卸载的设备")
		}
		// 系统盘的判断**以节点为准**：控制面不持有磁盘记录，凭设备名猜
		// （vda 一定是系统盘？）会在机型不同的机器上猜错，而猜错的结果是
		// 一台开不了机的虚拟机。
		disks, err := s.probeDisks(ctx, vm)
		if err != nil {
			return nil, err
		}
		for i := range disks {
			if disks[i].Dev != req.Dev {
				continue
			}
			if disks[i].IsSystem {
				return nil, api.ValidationFailed("系统盘不可卸载")
			}
			if running && !disks[i].Hotpluggable {
				return nil, api.Conflict(
					"该设备在运行中不支持热插拔，请先关机再卸载")
			}
		}

	case agent.DiskActionBus:
		if req.Dev == "" {
			return nil, api.InvalidParameter("请指定要调整的设备")
		}
		if !isBusAllowed(req.Bus) {
			return nil, api.InvalidParameter("不支持的总线类型")
		}
		if running {
			return nil, api.Conflict("换总线需要关机：来宾里的设备路径会变，运行中改会导致盘符漂移")
		}
		params["bus"] = req.Bus

	case agent.DiskActionMigrate:
		if req.Dev == "" {
			return nil, api.InvalidParameter("请指定要迁移的设备")
		}
		pool, err := s.loadMigrateTarget(ctx, vm.NodeID, req.TargetPoolID)
		if err != nil {
			return nil, err
		}
		if running && !req.AllowHot {
			return nil, api.Conflict(
				"虚拟机正在运行。热迁移期间磁盘仍在使用，业务会有抖动；确认请勾选「允许热迁移」，否则请先关机")
		}
		// 目标路径交给节点按池拼：控制面拼路径的话，池的挂载点一变
		// （换盘、迁移）这里算出来的路径就全错了。
		params["target_pool_id"] = pool.ID
		if pool.MountPath != nil {
			params["target_path"] = *pool.MountPath
		}
		params["hot"] = running && req.AllowHot
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDiskChange,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      derefOwner(vm.OwnerID),
		CreatedBy:    v.UserID,
		Params:       params,
		// 幂等键带上动作与设备：同一次点击的重复提交只产生一个任务，而
		// 「卸载 vdb 之后再挂载」不会被误判为重复。
		IdempotencyKey: "vm.disk.change:" + strconv.Itoa(int(vm.ID)) + ":" + req.Action + ":" + req.Dev,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.disk.change",
		Params: map[string]any{
			"action": req.Action, "dev": req.Dev,
			"bus": req.Bus, "file_id": req.FileID,
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// loadMigrateTarget 取出并校验一个迁移目标存储池。
//
// 池必须与虚拟机在同一节点：磁盘是一份具体的文件，跨节点"迁移"等于让另一
// 台机器去读一个不存在的路径。
func (s *Service) loadMigrateTarget(
	ctx context.Context, nodeID, poolID int64,
) (*model.StoragePool, error) {
	if poolID <= 0 {
		return nil, api.InvalidParameter("请选择目标存储池")
	}
	var pool model.StoragePool
	err := s.db.WithContext(ctx).Where("id = ?", poolID).First(&pool).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("存储池不存在")
	case err != nil:
		log.Printf("[vm] 查询存储池失败 id=%d: %v", poolID, err)
		return nil, api.Internal()
	}
	if pool.NodeID != nodeID {
		return nil, api.ValidationFailed("该存储池不在虚拟机所在的节点上")
	}
	if pool.Status != model.StoragePoolReady {
		return nil, api.ValidationFailed("目标存储池不可用")
	}
	return &pool, nil
}

// loadAttachable 取出并校验一个可挂载的磁盘文件。
func (s *Service) loadAttachable(
	ctx context.Context, nodeID, fileID int64, v authz.Viewer,
) (*model.StorageFile, error) {
	var file model.StorageFile
	err := s.db.WithContext(ctx).Where("id = ?", fileID).First(&file).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("磁盘文件不存在")
	case err != nil:
		log.Printf("[vm] 查询磁盘文件失败 id=%d: %v", fileID, err)
		return nil, api.Internal()
	}
	// 磁盘文件与虚拟机**必须在同一节点**：它是一份具体的文件，跨节点挂载
	// 等于让节点去读一个不存在的路径。
	if file.NodeID != nodeID {
		return nil, api.ValidationFailed("该磁盘文件不在虚拟机所在的节点上")
	}
	if file.Category != model.FileCategoryDisk {
		return nil, api.ValidationFailed("只能挂载「虚拟磁盘」类型的文件")
	}
	if !v.IsAdmin && file.UserID != nil && *file.UserID != v.UserID {
		return nil, api.NotFound("磁盘文件不存在")
	}
	return &file, nil
}

func isBusAllowed(bus string) bool {
	for _, o := range busOptions() {
		if o.Value == bus {
			return true
		}
	}
	return false
}

// DiskChangeExecutor 执行磁盘变更。
type DiskChangeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDiskChangeExecutor 构造执行器。
func NewDiskChangeExecutor(db *gorm.DB, client agent.Client) *DiskChangeExecutor {
	return &DiskChangeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DiskChangeExecutor) Type() string { return model.TaskVMDiskChange }

// Run 下发磁盘变更。
func (e *DiskChangeExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析磁盘变更参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMDiskChange,
		NodeID: nodeID,
		Target: strOf(p["vm_name"]),
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，磁盘未变更")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 换总线后同步控制面记录：vm.disk_bus 是「这台机器用哪种磁盘控制器」
	// 的事实，不同步的话详情页显示的是旧值，而节点上已经是新的了。
	if p["action"] == agent.DiskActionBus {
		if bus, ok := p["bus"].(string); ok && bus != "" && t.ResourceID != nil {
			if err := e.db.WithContext(ctx).Model(&model.VM{}).
				Where("id = ?", *t.ResourceID).Update("disk_bus", bus).Error; err != nil {
				// 只记日志：节点上已经改好了，控制面这一列只是展示用的投影，
				// 因为写它失败而把整个任务判失败会让用户以为没生效。
				log.Printf("[vm] 同步磁盘总线失败 vm=%d: %v", *t.ResourceID, err)
			}
		}
	}
	return nil
}
