// Package template 实现模板管理（F-3-01）与模板克隆（F-3-02）。
//
// 边界：本包只管**模板自身的生命周期**（制备、发布、删除）与克隆请求的受理；
// 克隆出来的虚拟机由 vm 包负责。两边通过 template_id 关联，不共享内部结构
// ——共用会让「模板加一个字段」意外影响虚拟机的创建路径。
package template

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
	"k_cockpit/internal/vm"
)

// Service 提供模板能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	audit *audit.Recorder
	// quota 校验存储配额（f-9-02）。允许为 nil（未启用配额的部署）。
	//
	// 复用 vm.QuotaChecker 而不是在本包再声明一个同形状的接口：两个接口
	// 长得一样却不共享，只会让人以为它们可以分别演化。本包本来就依赖 vm
	// 包（取状态文案），复用没有引入新的耦合。
	quota vm.QuotaChecker
	// agent 用于**实时探测**源虚拟机的运行态。
	//
	// 不能用投影判断「是否已关机」（f-2-01 R-002）：投影可能滞后，凭它
	// 就认定可以制备，会在一台运行中的机器上复制出一份崩溃一致性的模板
	// ——文件系统日志可能未提交，拿它建的每一台克隆机开机都要做 fsck。
	agent agent.Client
}

// NewService 构造模板服务。
func NewService(
	db *gorm.DB, queue *task.Queue, recorder *audit.Recorder, client agent.Client,
	quotaChecker vm.QuotaChecker,
) *Service {
	return &Service{
		db: db, queue: queue, audit: recorder, agent: client, quota: quotaChecker,
	}
}

// View 是模板的对外视图。
type View struct {
	ID         int64  `json:"id"`
	NodeID     int64  `json:"node_id"`
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Status     string `json:"status"`
	DiskFormat string `json:"disk_format"`
	DiskSizeGB int    `json:"disk_size_gb"`

	OSType    string `json:"os_type,omitempty"`
	OSVariant string `json:"os_variant,omitempty"`
	MinDiskGB int    `json:"min_disk_gb"`

	DefaultCPU      int `json:"default_cpu"`
	DefaultMemoryMB int `json:"default_memory_mb"`

	Published  bool   `json:"published"`
	Visibility string `json:"visibility"`
	// CloneEnabled 为 false 时不允许再克隆。
	CloneEnabled bool `json:"clone_enabled"`
	// Immutable 为 true 时模板盘以只读方式共享。
	Immutable bool `json:"immutable"`

	// ParentID 非空表示它是从另一个模板派生的（链式克隆的父级）。
	ParentID *int64 `json:"parent_id,omitempty"`
	// Error 仅在制备失败时有值。
	Error string `json:"error,omitempty"`

	Remark    string `json:"remark,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ListFilter 是模板查询条件。
type ListFilter struct {
	NodeID int64
	// Keyword 按名称模糊匹配。
	Keyword string
	// OnlyReady 只返回可用于创建的模板。
	OnlyReady bool
	Viewer    authz.Viewer
}

// List 返回可见的模板。
//
// 可见性规则：管理员看到全部；普通用户看到**已发布**的，加上自己创建的
// 私有模板。这两类是分开的——「已发布」是共享意愿，「自己创建的」是所有权，
// 只有前者需要经过发布这一步。
func (s *Service) List(ctx context.Context, f ListFilter) ([]View, error) {
	query := s.db.WithContext(ctx).Model(&model.Template{})

	if f.NodeID > 0 {
		query = query.Where("node_id = ?", f.NodeID)
	}
	if f.OnlyReady {
		query = query.Where("status = ?", model.TemplateReady)
	}
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		query = query.Where("name ILIKE ?", "%"+kw+"%")
	}

	if !f.Viewer.IsAdmin {
		// 已发布 + 自己创建的。admin 不受这两条限制。
		query = query.Where("published = ? OR created_by = ?", true, f.Viewer.UserID)
	}

	var rows []model.Template
	if err := query.Order("id DESC").Find(&rows).Error; err != nil {
		log.Printf("[template] 查询模板失败: %v", err)
		return nil, api.Internal()
	}

	views := make([]View, 0, len(rows))
	for i := range rows {
		views = append(views, toView(&rows[i]))
	}
	return views, nil
}

// Get 返回模板详情。
func (s *Service) Get(ctx context.Context, id int64, v authz.Viewer) (*View, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	view := toView(tpl)
	return &view, nil
}

// CreateFromVMRequest 是从虚拟机制备模板的请求。
type CreateFromVMRequest struct {
	VMID int64
	Name string
	// OSType / OSVariant 供界面展示与筛选，也为「重装系统」提供判断依据。
	OSType    string
	OSVariant string
	Remark    string
	// Published 表示制备完成后立即发布给其他用户。
	Published bool
}

// CreateFromVM 从一台虚拟机的系统盘制备模板（F-3-01）。
//
// **必须先关机**，理由与快照不同：快照（f-2-07）要求的是数据一致性，而
// 模板会被**反复克隆成新机器**——一个崩溃一致性的模板意味着每一台克隆机
// 开机时都要做 fsck，运气不好就是只读挂载。这类问题会以「克隆机启动后
// 文件系统只读」的形式出现，而几乎不可能被追溯到模板制备的那一刻。
func (s *Service) CreateFromVM(
	ctx context.Context, req CreateFromVMRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 64 {
		return nil, api.InvalidParameter("模板名称需为 1-64 个字符")
	}
	if req.VMID <= 0 {
		return nil, api.InvalidParameter("必须指定源虚拟机")
	}

	var vmRow model.VM
	err := s.db.WithContext(ctx).Where("id = ?", req.VMID).First(&vmRow).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("虚拟机不存在")
	case err != nil:
		log.Printf("[template] 查询源虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	// 归属检查在服务层做一次，避免 tenant 拿别人的虚拟机去制备模板。
	if !v.IsAdmin && (vmRow.OwnerID == nil || *vmRow.OwnerID != v.UserID) {
		// 404 而非 403：403 会确认「这个 ID 存在」。
		return nil, api.NotFound("虚拟机不存在")
	}
	if !vmRow.Present {
		return nil, api.ValidationFailed("该虚拟机已不在虚拟化层")
	}

	// **必须先关机**：运行中的系统盘在被复制的同时还在被写入，复制出来的
	// 模板会是一个崩溃一致性的快照。它不是「可能有点脏」，而是会让**每一台**
	// 克隆机开机时都要做 fsck——运气不好就是只读挂载。而这类现象几乎不可能
	// 被追溯到模板制备的那一刻。
	if s.agent != nil {
		result, err := s.agent.Execute(ctx, agent.Operation{
			Kind: agent.OpVMStatus, NodeID: vmRow.NodeID, Target: vmRow.Name,
		})
		if err != nil {
			return nil, api.Unavailable("节点不可达，无法确认源虚拟机状态")
		}
		if !result.Success {
			return nil, api.ValidationFailed(result.Message)
		}
		status, _ := result.Data[agent.StatusDataKey].(string)
		if status != model.VMStatusStopped {
			return nil, api.ValidationFailed(
				"制备模板需要源虚拟机关机（当前：" + vm.DescribeStatus(status) + "）")
		}
	}

	// 名字唯一性先查一次，给出一句能看懂的话；唯一索引仍然在写入时兜底。
	var dup int64
	if err := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("node_id = ? AND name = ?", vmRow.NodeID, req.Name).
		Count(&dup).Error; err != nil {
		log.Printf("[template] 检查模板重名失败: %v", err)
		return nil, api.Internal()
	}
	if dup > 0 {
		return nil, api.Conflict("该节点上已有同名模板")
	}

	// 模板是一份磁盘副本，占用与虚拟机磁盘同级——因此同样计入配额。
	// 用源虚拟机的磁盘大小作估算：模板大小与它几乎一致。
	if s.quota != nil {
		if err := s.quota.Check(ctx, v.UserID, vmRow.NodeID,
			int64(vmRow.DiskGB)*1024*1024*1024); err != nil {
			return nil, err
		}
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type: model.TaskTemplatePrepare,
		// 资源锁键是 vm:<id>：制备要读系统盘，必须与电源操作、快照串行，
		// 否则「复制到一半虚拟机被开机」会产出不一致的模板。
		NodeID:       vmRow.NodeID,
		ResourceType: "vm",
		ResourceID:   vmRow.ID,
		ResourceName: req.Name,
		OwnerID:      ownerOf(vmRow, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id":      vmRow.ID,
			"vm_name":    vmRow.Name,
			"name":       req.Name,
			"node_id":    vmRow.NodeID,
			"os_type":    req.OSType,
			"os_variant": req.OSVariant,
			"remark":     req.Remark,
			"published":  req.Published,
			// 默认硬件参数取自源虚拟机——这是最合理的猜测，用户可以在
			// 克隆时覆盖。不取的话每次克隆都要重新填一遍。
			"default_cpu":       vmRow.VCPU,
			"default_memory_mb": vmRow.MemoryMB,
			"disk_size_gb":      vmRow.DiskGB,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.prepare", vmRow.NodeID, req.Name, t.ID)
	return t, nil
}

// UpdateRequest 修改模板的展示属性。
type UpdateRequest struct {
	Published    *bool
	Visibility   string
	CloneEnabled *bool
	Remark       string
}

// Update 修改模板（发布 / 下线克隆 / 备注）。
//
// 这些都是**纯控制面元数据**，因此同步生效、不进任务队列：它们不影响
// 宿主机上的任何东西。做成任务会制造一个「界面说已发布、实际还没发布」
// 的窗口，而发布与否决定了别人能不能用——那是最不该含糊的一类状态。
func (s *Service) Update(
	ctx context.Context, id int64, req UpdateRequest, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	if req.Published != nil {
		updates["published"] = *req.Published
	}
	if req.CloneEnabled != nil {
		updates["clone_enabled"] = *req.CloneEnabled
	}
	if req.Visibility != "" {
		updates["visibility"] = req.Visibility
	}
	if req.Remark != "" {
		updates["remark"] = req.Remark
	}
	if len(updates) == 0 {
		view := toView(tpl)
		return &view, nil
	}

	if err := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("id = ?", tpl.ID).Updates(updates).Error; err != nil {
		log.Printf("[template] 更新模板失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.update", tpl.NodeID, tpl.Name, 0)

	updated, err := s.Get(ctx, id, v)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Delete 删除模板。
//
// **链式克隆的存在会阻止删除**：linked 克隆体的磁盘只是一个 overlay，
// 父盘一删，那些虚拟机的数据就不可用了——而且不会立刻报错，要等到下次
// 开机或读某个未缓存的数据块时才暴露。
//
// 因此这里同步检查并拒绝，同时告诉用户有几个克隆体、以及可以先做什么
// （把那些克隆体转为独立虚拟机，f-3-02）。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	var linked int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("template_id = ? AND clone_mode = ? AND present = ?", tpl.ID, model.CloneLinked, true).
		Count(&linked).Error; err != nil {
		log.Printf("[template] 统计链式克隆失败: %v", err)
		return nil, api.Internal()
	}
	if linked > 0 {
		return nil, api.Conflict(
			"仍有 " + strconv.FormatInt(linked, 10) + " 台链式克隆的虚拟机依赖这个模板，删除会让它们的数据不可用；" +
				"请先把它们转为独立虚拟机")
	}

	// 派生模板（以它作为父级的）也要一并拒绝：链断在中间同样会让下游失效。
	var children int64
	if err := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("parent_id = ?", tpl.ID).Count(&children).Error; err != nil {
		log.Printf("[template] 统计派生模板失败: %v", err)
		return nil, api.Internal()
	}
	if children > 0 {
		return nil, api.Conflict(
			"仍有 " + strconv.FormatInt(children, 10) + " 个模板以它为父级，请先处理这些派生模板")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskTemplateDelete,
		NodeID:       tpl.NodeID,
		ResourceType: "template",
		ResourceID:   tpl.ID,
		ResourceName: tpl.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"template_id": tpl.ID,
			"name":        tpl.Name,
			"disk_path":   tpl.DiskPathOf(),
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.delete", tpl.NodeID, tpl.Name, t.ID)
	return t, nil
}

// load 读取模板并做可见性检查。
func (s *Service) load(ctx context.Context, id int64, v authz.Viewer) (*model.Template, error) {
	var tpl model.Template
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&tpl).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("模板不存在")
	case err != nil:
		log.Printf("[template] 查询模板失败: %v", err)
		return nil, api.Internal()
	}

	// 可见性检查用**404 而不是 403**：403 会确认「这个 ID 存在」，
	// 让 tenant 能通过枚举推断出别人有多少模板。
	if !v.IsAdmin && !tpl.Published && (tpl.CreatedBy == nil || *tpl.CreatedBy != v.UserID) {
		return nil, api.NotFound("模板不存在")
	}
	return &tpl, nil
}

func toView(tpl *model.Template) View {
	view := View{
		ID: tpl.ID, NodeID: tpl.NodeID, Name: tpl.Name, Version: tpl.Version,
		Status: tpl.Status, DiskFormat: tpl.DiskFormat, DiskSizeGB: tpl.DiskSizeGB,
		MinDiskGB: tpl.MinDiskGB, DefaultCPU: tpl.DefaultCPU,
		DefaultMemoryMB: tpl.DefaultMemoryMB, Published: tpl.Published,
		Visibility: tpl.Visibility, CloneEnabled: tpl.CloneEnabled,
		Immutable: tpl.Immutable, ParentID: tpl.ParentID,
		CreatedAt: tpl.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if tpl.OSType != nil {
		view.OSType = *tpl.OSType
	}
	if tpl.OSVariant != nil {
		view.OSVariant = *tpl.OSVariant
	}
	if tpl.Error != nil {
		view.Error = *tpl.Error
	}
	if tpl.Remark != nil {
		view.Remark = *tpl.Remark
	}
	return view
}

func (s *Service) record(
	ctx context.Context, operatorID int64, operatorName, clientIP, action string,
	nodeID int64, name string, taskID int64,
) {
	if s.audit == nil {
		return
	}
	params := map[string]any{}
	if taskID > 0 {
		params["task_id"] = taskID
	}
	s.audit.Record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "template", ResourceName: name,
		Action: action, Params: params, Success: true, ClientIP: clientIP,
	})
}

// ownerOf 返回任务归属：优先资源所有者，其次操作者。
func ownerOf(vm model.VM, v authz.Viewer) int64 {
	if vm.OwnerID != nil {
		return *vm.OwnerID
	}
	return v.UserID
}
