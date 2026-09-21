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
	// 默认硬件：克隆时的默认值，取自源虚拟机。为空表示沿用系统默认。
	DefaultDiskBus     string `json:"default_disk_bus,omitempty"`
	DefaultNicModel    string `json:"default_nic_model,omitempty"`
	DefaultMachineType string `json:"default_machine_type,omitempty"`
	DefaultFirmware    string `json:"default_firmware,omitempty"`
	// DefaultVideoModel 取自源虚拟机的显示设备类型。
	DefaultVideoModel string `json:"default_video_model,omitempty"`

	Published  bool   `json:"published"`
	Visibility string `json:"visibility"`
	// CloneEnabled 为 false 时不允许再克隆。
	CloneEnabled bool `json:"clone_enabled"`
	// Immutable 为 true 时模板盘以只读方式共享。
	Immutable bool `json:"immutable"`

	// ParentID 非空表示它是从另一个模板派生的（链式克隆的父级）。
	ParentID *int64 `json:"parent_id,omitempty"`
	// FamilyID 是这条派生链的**根模板 ID**。它与 version 一起回答"这是第
	// 几代、和谁是同一族"，界面据此把同一族的多个版本收在一起。
	FamilyID *int64 `json:"family_id,omitempty"`
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
	// ParentID 非空表示这是**某个模板的新版本**（F-3-04）：新模板会挂到
	// 同一族上，版本号在该族内自增。
	//
	// 它不改变制备动作本身（仍然是复制这台机器的系统盘），只改变新模板在
	// 族里的位置——同一个模板可以有 v1（基础系统）与 v2（装好了运行时），
	// 而用户要能看出后者是从前者来的。
	ParentID *int64
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

	// 派生制备：确定族与版本号。
	var familyID *int64
	version := 1
	if req.ParentID != nil {
		parent, err := s.load(ctx, *req.ParentID, v)
		if err != nil {
			return nil, err
		}
		// 派生必须同节点：模板盘就在那个节点的存储池里，跨节点的"新版本"
		// 无法复用它的 backing 链，也无从保证内容真的来自那个父模板。
		if parent.NodeID != vmRow.NodeID {
			return nil, api.ValidationFailed("父模板不在同一节点上，无法作为新版本的来源")
		}
		if parent.Status != model.TemplateReady {
			return nil, api.ValidationFailed("父模板尚未就绪，不能派生新版本")
		}
		fid := parent.ID
		if parent.FamilyID != nil {
			fid = *parent.FamilyID
		}
		familyID = &fid
		version, err = s.nextVersion(ctx, fid)
		if err != nil {
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
			"default_cpu":          vmRow.VCPU,
			"default_memory_mb":    vmRow.MemoryMB,
			"disk_size_gb":         vmRow.DiskGB,
			"default_disk_bus":     vmRow.DiskBus,
			"default_nic_model":    vmRow.NicModel,
			"default_machine_type": vmRow.MachineType,
			"default_firmware":     vmRow.Firmware,
			"default_display":      vmRow.DisplayDevice,
			// 族与版本：只有派生制备才有值，独立制备时留空（version 仍为 1）。
			"parent_id": req.ParentID,
			"family_id": familyID,
			"version":   version,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.prepare", vmRow.NodeID, req.Name, t.ID)
	return t, nil
}

// nextVersion 返回该族的下一个版本号。
//
// 取**族内最大值 + 1**而不是"父版本 + 1"：同一个父下派生两次应当得到
// v2 与 v3；按父版本算则两者都叫 v2，而名字唯一约束会让第二次制备以一个
// 看不懂的冲突失败。
func (s *Service) nextVersion(ctx context.Context, familyID int64) (int, error) {
	var maxVersion int
	if err := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("family_id = ?", familyID).
		Select("COALESCE(MAX(version), 0)").Scan(&maxVersion).Error; err != nil {
		log.Printf("[template] 计算版本号失败 family=%d: %v", familyID, err)
		return 0, api.Internal()
	}
	return maxVersion + 1, nil
}

// Family 返回同一个模板族里的全部模板（按版本排序）。
//
// 前端用它渲染"这一族有几个版本"，删除策略用它预览会波及哪些。排序按
// version 而不是 created_at：版本号才是"第几代"，而制备时间可能因为重试
// 而倒挂。
func (s *Service) Family(ctx context.Context, id int64, v authz.Viewer) ([]View, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	fid := tpl.ID
	if tpl.FamilyID != nil {
		fid = *tpl.FamilyID
	}

	var rows []model.Template
	if err := s.db.WithContext(ctx).
		Where("id = ? OR family_id = ?", fid, fid).
		Order("version ASC, id ASC").Find(&rows).Error; err != nil {
		log.Printf("[template] 查询模板族失败 family=%d: %v", fid, err)
		return nil, api.Internal()
	}
	out := make([]View, 0, len(rows))
	for i := range rows {
		out = append(out, toView(&rows[i]))
	}
	return out, nil
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

// 删除派生链的策略。
const (
	// DeleteStrategyCascade 连同整条派生链一起删除。
	DeleteStrategyCascade = "cascade"
	// DeleteStrategyPromote 把下一级提升为独立模板（挂到自己的父级上），
	// 只删这一个。
	//
	// 它对应一个真实需求：v1 已经过时，但 v2 / v3 还在被使用——此时"删掉
	// 整条链"是错的，而"因为下游还在就不能删"同样不对。
	DeleteStrategyPromote = "promote"
)

// DeleteRequest 是一次删除请求。
type DeleteRequest struct {
	// Strategy 仅在存在派生模板时有效，取值见 DeleteStrategy*。
	Strategy string
}

// Delete 删除模板。
//
// **链式克隆的存在会阻止删除**：linked 克隆体的磁盘只是一个 overlay，
// 父盘一删，那些虚拟机的数据就不可用了——而且不会立刻报错，要等到下次
// 开机或读某个未缓存的数据块时才暴露。这类依赖**没有策略可以绕过**：
// 任何策略都意味着接受数据丢失，那不该由一次点击决定。
//
// 派生模板则不同：它是一条版本链，删掉中间一代有两种合理做法（级联、提
// 升），因此把选择交给用户，并由 Strategy 显式表达。
func (s *Service) Delete(
	ctx context.Context, id int64, req DeleteRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	// **判定与预览共用同一段代码。** 两处各写一遍的话迟早分叉，而分叉的表现
	// 是「预览说可以删、点下去却报冲突」——用户会以为界面上那个绿色的
	// 「可以删除」在骗他，而这恰恰是预览存在意义的反面。
	blockers, err := s.deleteBlockers(ctx, tpl)
	if err != nil {
		return nil, err
	}

	ids := []int64{tpl.ID}
	paths := []string{tpl.DiskPathOf()}

	for _, b := range blockers {
		if b.Kind == "linked_vm" {
			return nil, api.Conflict(b.message())
		}
	}
	if len(blockers) > 0 {
		switch req.Strategy {
		case DeleteStrategyCascade:
			children, err := s.descendants(ctx, tpl.ID)
			if err != nil {
				return nil, err
			}
			for i := range children {
				ids = append(ids, children[i].ID)
				if children[i].DiskPath != nil && *children[i].DiskPath != "" {
					paths = append(paths, *children[i].DiskPath)
				}
			}
		case DeleteStrategyPromote:
			if err := s.promoteChildren(ctx, tpl); err != nil {
				return nil, err
			}
		default:
			return nil, api.Conflict(blockers[0].message())
		}
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
			"template_id":  tpl.ID,
			"name":         tpl.Name,
			"disk_path":    tpl.DiskPathOf(),
			"template_ids": ids,
			"disk_paths":   paths,
			"strategy":     req.Strategy,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.delete", tpl.NodeID, tpl.Name, t.ID)
	return t, nil
}

// promoteChildren 把直接子级挂到自己的父级上，让它们成为独立的一支。
//
// 族不变：它们仍然是"同一个模板的后代"，只是不再依赖被删掉的这一代。
// 把族也拆开会丢失"这些版本同源"这条信息，而那正是族存在的意义。
func (s *Service) promoteChildren(ctx context.Context, tpl *model.Template) error {
	res := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("parent_id = ?", tpl.ID).
		Updates(map[string]any{"parent_id": tpl.ParentID})
	if res.Error != nil {
		log.Printf("[template] 提升子模板失败 id=%d: %v", tpl.ID, res.Error)
		return api.Internal()
	}
	return nil
}

// load 读取模板并做可见性检查。
// Blocker 是一条阻止删除模板的依赖。
//
// **带上具体名字而不是只给计数。** 「仍有 3 台链式克隆依赖它」只告诉用户
// 有麻烦，而没说找谁；他要拿这个去处理，就必须知道是哪几台。计数与名单
// 都能给，而名单才是能用的那个。
type Blocker struct {
	// Kind 是依赖类型：linked_vm / child_template。
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	Count int      `json:"count"`
	Names []string `json:"names"`
	// Unit 是量词（台 / 个）。单独一个字段而不是拼进 Label：「链式克隆的
	// 虚拟机」作为表格标题合适，而「仍有 2 链式克隆的虚拟机」缺个量词。
	Unit string `json:"unit"`
	// Fix 写清「怎么解决」，而不是只说「不行」。
	Fix string `json:"fix"`
}

func (b Blocker) message() string {
	return "仍有 " + strconv.Itoa(b.Count) + " " + b.Unit + b.Label +
		"依赖这个模板；" + b.Fix
}

// DeletePreviewView 是删除前的预览（F-3-01）。
type DeletePreviewView struct {
	CanDelete bool      `json:"can_delete"`
	Blockers  []Blocker `json:"blockers"`
	// DiskPath 是删除后会释放的磁盘文件，让用户知道「删掉的是什么」。
	DiskPath string `json:"disk_path"`
	// LinkedVMCount / ChildCount 是两项依赖各自的计数。
	//
	// 它们与 Blockers 里的 count 是同一个数，但**可删除时 Blockers 为空**
	// ——那时预览里仍要说明「没有依赖，删掉只是释放磁盘」。
	LinkedVMCount int `json:"linked_vm_count"`
	ChildCount    int `json:"child_template_count"`
	// Strategies 是存在派生模板时可选的删除策略。
	//
	// 由服务端下发而不是前端按 blocker 猜：策略能不能用取决于依赖的
	// **种类**（链式克隆不可绕过、派生链可以级联或提升），而这个判断只有
	// 服务端做得完整。
	Strategies []string `json:"strategies"`
}

// DeletePreview 返回删除前的检查结果（API-033）。
//
// 它存在的理由：这些约束**本来就有**（Delete 里拒绝），但用户只有在点了
// 删除之后才会撞上——而那时他看到的是一个错误提示，不是一份待办清单。
// 预览把这件工作在「按下按钮之前」完成。
func (s *Service) DeletePreview(ctx context.Context, id int64, v authz.Viewer) (*DeletePreviewView, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	blockers, err := s.deleteBlockers(ctx, tpl)
	if err != nil {
		return nil, err
	}

	view := &DeletePreviewView{
		CanDelete: len(blockers) == 0,
		Blockers:  blockers,
		DiskPath:  tpl.DiskPathOf(),
	}
	for _, b := range blockers {
		switch b.Kind {
		case "linked_vm":
			view.LinkedVMCount = b.Count
		case "child_template":
			view.ChildCount = b.Count
			view.Strategies = []string{DeleteStrategyCascade, DeleteStrategyPromote}
		}
	}
	return view, nil
}

// deleteBlockers 找出阻止删除的全部依赖。
//
// **一次返回全部，而不是撞到第一个就返回。** 用户要处理的是一个清单：
// 只报第一项的话，他解决了链式克隆再点一次，又会撞上派生模板——两次往返
// 换来的信息本可以一次给全。
func (s *Service) deleteBlockers(ctx context.Context, tpl *model.Template) ([]Blocker, error) {
	var blockers []Blocker

	// 链式克隆：它们的磁盘是**这个模板的派生**，删模板会让数据不可用。
	var vms []model.VM
	if err := s.db.WithContext(ctx).
		Select("id", "name").
		Where("template_id = ? AND clone_mode = ? AND present = ?",
			tpl.ID, model.CloneLinked, true).
		Limit(nameListLimit).
		Find(&vms).Error; err != nil {
		log.Printf("[template] 统计链式克隆失败: %v", err)
		return nil, api.Internal()
	}
	if len(vms) > 0 {
		names := make([]string, 0, len(vms))
		for i := range vms {
			names = append(names, vms[i].Name)
		}
		blockers = append(blockers, Blocker{
			Kind: "linked_vm", Label: "链式克隆的虚拟机", Unit: "台",
			Count: len(vms), Names: names,
			Fix: "删除会让它们的数据不可用；请先把它们转为独立虚拟机（在虚拟机详情页的磁盘区）。",
		})
	}

	// 派生模板：链断在中间同样会让下游失效。
	//
	// 统计的是**整棵子树**而不只是直接子级：v2 是从 v1 派生的、v3 又从
	// v2 派生，删掉 v1 时只提示"v2 依赖它"会让用户在处理完 v2 之后才
	// 发现还有 v3——那正是"撞到第一个就返回"的坏处。
	children, err := s.descendants(ctx, tpl.ID)
	if err != nil {
		return nil, err
	}
	if len(children) > 0 {
		names := make([]string, 0, len(children))
		for i := range children {
			if len(names) >= nameListLimit {
				break
			}
			names = append(names, children[i].Name)
		}
		blockers = append(blockers, Blocker{
			Kind: "child_template", Label: "派生自它的模板（含间接派生）", Unit: "个",
			Count: len(children), Names: names,
			Fix: "可级联删除整条派生链，或把它的下一级提升为独立模板后再删。",
		})
	}

	return blockers, nil
}

// descendants 返回以某模板为根的全部派生模板（含间接），不含它自己。
//
// 逐层展开而不是递归查询：一次 SQL 递归（WITH RECURSIVE）在两种数据库上
// 写法不同，而这条链的深度实际上是**个位数**——模板是人工制备的，不会有
// 几百层。用几次简单查询换一个两种数据库都能跑的实现更划算。
func (s *Service) descendants(ctx context.Context, rootID int64) ([]model.Template, error) {
	out := []model.Template{}
	level := []int64{rootID}
	seen := map[int64]bool{rootID: true}

	for len(level) > 0 {
		var rows []model.Template
		if err := s.db.WithContext(ctx).
			Select("id", "name", "disk_path", "parent_id").
			Where("parent_id IN ?", level).
			Find(&rows).Error; err != nil {
			log.Printf("[template] 查询派生模板失败: %v", err)
			return nil, api.Internal()
		}
		next := make([]int64, 0, len(rows))
		for i := range rows {
			if seen[rows[i].ID] {
				continue // 数据异常导致的环：宁可停下，也不无限展开
			}
			seen[rows[i].ID] = true
			out = append(out, rows[i])
			next = append(next, rows[i].ID)
		}
		level = next
	}
	return out, nil
}

// nameListLimit 限制预览里列出的名字条数。
//
// 上限本身就是信息：真出现几百台依赖时，预览页面不该变成一份几千行的
// 名单。超出时 Count 仍是真实总数，只是名字只列前若干条。
const nameListLimit = 50

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
	view.FamilyID = tpl.FamilyID
	if tpl.DefaultDiskBus != nil {
		view.DefaultDiskBus = *tpl.DefaultDiskBus
	}
	if tpl.DefaultNicModel != nil {
		view.DefaultNicModel = *tpl.DefaultNicModel
	}
	if tpl.DefaultMachineType != nil {
		view.DefaultMachineType = *tpl.DefaultMachineType
	}
	if tpl.DefaultFirmware != nil {
		view.DefaultFirmware = *tpl.DefaultFirmware
	}
	if tpl.DefaultVideoModel != nil {
		view.DefaultVideoModel = *tpl.DefaultVideoModel
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
