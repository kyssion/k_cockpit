package vm

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// 可编辑字段的键。
const (
	EditFieldVCPU      = "vcpu"
	EditFieldMemoryMB  = "memory_mb"
	EditFieldRemark    = "remark"
	EditFieldGroupName = "group_name"
)

// 子选项卡标识，与 FRONTEND.md §5.3.3 的「编辑」子选项卡对应。
const (
	EditGroupBasic    = "basic"
	EditGroupDisk     = "disk"
	EditGroupBoot     = "boot"
	EditGroupNetwork  = "network"
	EditGroupPassthru = "passthru"
	EditGroupAdvanced = "advanced"
)

// EditField 描述一个可编辑的配置项。
type EditField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Kind 决定界面用什么控件：number / text。
	Kind string `json:"kind"`
	// Group 是所属的子选项卡。
	Group string `json:"group"`

	// RequiresNode 表示修改这一项需要**下发到节点**。
	//
	// 为 false 的是纯控制面元数据（备注、分组）：虚拟化层根本不知道有这两个
	// 概念，改它们不需要任何节点操作，因此也**不受运行态限制**——
	// 虚拟机开着也能改备注。
	RequiresNode bool `json:"requires_node"`

	// RequiresShutdown 表示修改这一项必须先关机。
	// 仅在 RequiresNode 为 true 时有意义。
	RequiresShutdown bool `json:"requires_shutdown"`

	Min  int    `json:"min,omitempty"`
	Max  int    `json:"max,omitempty"`
	Hint string `json:"hint,omitempty"`
}

// editFields 是**运行态可改矩阵**（F-2-05）。
//
// 它是这个功能的**单一事实来源**：界面按它渲染控件与「可热改 / 需关机」标记，
// 后端按它校验提交。前后端各写一份的话，用户会在界面上看到「可热改」、
// 提交后却被后端以「需要关机」拒绝——这类不一致最伤信任，而且只有真正
// 动手操作的用户才会碰到，测试很难覆盖。
var editFields = []EditField{
	{
		Key: EditFieldVCPU, Label: "CPU 核数", Kind: "number", Group: EditGroupBasic,
		RequiresNode: true, RequiresShutdown: true,
		Min: 1, Max: 64,
		Hint: "需要关机后修改，开机时调整无法保证来宾一致。",
	},
	{
		Key: EditFieldMemoryMB, Label: "内存（MB）", Kind: "number", Group: EditGroupBasic,
		RequiresNode: true, RequiresShutdown: true,
		Min: 128, Max: 262144,
		Hint: "需要关机后修改。",
	},
	{
		Key: EditFieldRemark, Label: "备注", Kind: "text", Group: EditGroupBasic,
		// 纯元数据：只存在于控制面，改它不需要碰虚拟化层，
		// 因此**没有 RequiresShutdown**。
		RequiresNode: false,
		Hint:         "仅记录在控制面，不影响虚拟机运行。",
	},
	{
		Key: EditFieldGroupName, Label: "分组", Kind: "text", Group: EditGroupBasic,
		RequiresNode: false,
		Hint:         "用于列表筛选，仅记录在控制面。",
	},
}

// EditForm 是编辑页的表单元数据与当前值。
type EditForm struct {
	// Fields 是**全部**可编辑项（含那些需要关机才能改的）。
	//
	// 不按运行态过滤：用户需要看到「有哪些项可以改」，只是暂时改不了。
	// 直接隐藏会让他在关机之后再进来才发现多出几项，从而怀疑自己记错了。
	Fields []EditField `json:"fields"`
	// Values 是各项的当前值。
	Values map[string]any `json:"values"`
	// EditableNow 报告**以当前运行态**能否提交需要下发的修改。
	//
	// 为 false 时界面应禁用「保存」并说明原因（需先关机），而不是让用户
	// 填完一屏再被拒绝。
	EditableNow bool `json:"editable_now"`
	// CurrentStatus 是做出上述判断所依据的状态。
	CurrentStatus string `json:"current_status"`
}

// EditFormOf 返回某台虚拟机的编辑表单元数据与当前值。
//
// **实时探测**运行态（而不是读投影）：EditableNow 直接决定界面能否提交，
// 用陈旧的投影可能导致用户解除了按钮禁用、提交后才被拒绝。
func (s *Service) EditFormOf(
	ctx context.Context, vmID int64, v authz.Viewer,
) (*EditForm, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	return &EditForm{
		Fields:        editFields,
		Values:        editValues(vm),
		EditableNow:   current == model.VMStatusStopped,
		CurrentStatus: current,
	}, nil
}

// editValues 取各项当前值。
func editValues(vm *model.VM) map[string]any {
	return map[string]any{
		EditFieldVCPU:      vm.VCPU,
		EditFieldMemoryMB:  vm.MemoryMB,
		EditFieldRemark:    derefStr(vm.Remark),
		EditFieldGroupName: derefStr(vm.GroupName),
	}
}

// UpdateMetadataRequest 是元数据修改请求。
//
// 用指针表示「本次是否提交了这一项」：只有提交的项才会被写入，未提交的项
// 保持原值。用值类型会让「清空备注」与「不修改备注」无法区分——前者是用户
// 明确意图，后者是没碰过这个输入框。
type UpdateMetadataRequest struct {
	Remark    *string
	GroupName *string
}

// UpdateMetadata 直接更新控制面元数据（备注、分组）。
//
// **不入队、不探测、不校验运行态**：这两项只存在于我们的数据库里，虚拟化层
// 根本不知道它们，因此没有「运行中不能改」这回事。让它们和硬件配置走同一条
// 路径，会让改个备注也要等一次节点往返。
func (s *Service) UpdateMetadata(
	ctx context.Context, vmID int64, req UpdateMetadataRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	before := map[string]any{}

	if req.Remark != nil {
		trimmed := strings.TrimSpace(*req.Remark)
		updates["remark"] = optStr(trimmed)
		before["remark"] = derefStr(vm.Remark)
	}
	if req.GroupName != nil {
		trimmed := strings.TrimSpace(*req.GroupName)
		updates["group_name"] = optStr(trimmed)
		before["group_name"] = derefStr(vm.GroupName)
	}

	if len(updates) == 0 {
		return nil, api.InvalidParameter("没有需要修改的内容")
	}

	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vm.ID).Updates(updates).Error; err != nil {
		log.Printf("[vm] 更新元数据失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:      "vm.metadata.update",
		Params:      updates,
		BeforeState: before,
		AfterState:  updates,
		Success:     true, ClientIP: clientIP,
	})

	updated, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	view := toView(updated, time.Now(), s.staleThreshold())
	return &view, nil
}

// ConfigChangeRequest 是一次硬件配置变更请求。
//
// 与 UpdateMetadataRequest 一样用指针区分「是否提交了这一项」。
type ConfigChangeRequest struct {
	VCPU     *int
	MemoryMB *int
}

// UpdateConfig 受理一次硬件配置变更。
//
// 三项校验缺一不可：
//  1. 字段在**矩阵**里且允许修改（矩阵是单一事实来源，不在其中的字段直接拒绝）；
//  2. **实时探测**运行态——矩阵里标了 RequiresShutdown 的项要求已关机；
//  3. 变更内容非空。
//
// 校验通过后入队：真正的执行由任务队列按资源锁串行（与电源操作共用 vm:<id>，
// 因此不会出现「边改配置边开机」）。
func (s *Service) UpdateConfig(
	ctx context.Context, vmID int64, req ConfigChangeRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 只收集**真正变化**的字段（差异提交，F-2-05）。
	//
	// 这与界面上的「只提交变化字段」是同一件事的两端：界面负责不把没动过的
	// 输入框发上来，这里负责再筛一遍。两端都做不是冗余——界面可能因为用户
	// 改了又改回来而发上等值字段，而「等值修改」会白白触发一次重启。
	changes := map[string]any{}
	if req.VCPU != nil && *req.VCPU != vm.VCPU {
		if err := validateRange(EditFieldVCPU, *req.VCPU); err != nil {
			return nil, err
		}
		changes[EditFieldVCPU] = *req.VCPU
	}
	if req.MemoryMB != nil && *req.MemoryMB != vm.MemoryMB {
		if err := validateRange(EditFieldMemoryMB, *req.MemoryMB); err != nil {
			return nil, err
		}
		changes[EditFieldMemoryMB] = *req.MemoryMB
	}

	if len(changes) == 0 {
		return nil, api.InvalidParameter("没有需要修改的内容")
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}

	// 需要关机的字段在运行态下一律拒绝。
	//
	// 拒绝而不是「自动关机再改」：自动关机会中断用户正在跑的业务，而那是
	// 一个他没有要求的操作。让他自己决定什么时候停机。
	if current != model.VMStatusStopped {
		if requiresShutdown(changes) {
			return nil, api.ValidationFailed(
				"修改 CPU 或内存需要先关机（当前为" + DescribeStatus(current) + "）")
		}
	}

	params := configChangeParams{
		VMID: vm.ID, VMName: vm.Name,
		Changes:        changes,
		ObservedStatus: current,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMConfigUpdate,
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

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.config.update.request",
		Params: params,
		BeforeState: map[string]any{
			EditFieldVCPU: vm.VCPU, EditFieldMemoryMB: vm.MemoryMB,
		},
		AfterState: map[string]any{"changes": changes, "task_id": t.ID},
		Success:    true, ClientIP: clientIP,
	})
	return t, nil
}

// validateRange 校验数值字段是否在矩阵定义的范围内。
func validateRange(key string, value int) error {
	for _, f := range editFields {
		if f.Key != key {
			continue
		}
		if f.Min > 0 && value < f.Min {
			return api.InvalidParameter(f.Label + "不能小于 " + strconv.Itoa(f.Min))
		}
		if f.Max > 0 && value > f.Max {
			return api.InvalidParameter(f.Label + "不能大于 " + strconv.Itoa(f.Max))
		}
		return nil
	}
	return api.InvalidParameter("不支持的配置项: " + key)
}

// requiresShutdown 报告变更集合中是否有需要关机的字段。
func requiresShutdown(changes map[string]any) bool {
	for key := range changes {
		for _, f := range editFields {
			if f.Key == key && f.RequiresShutdown {
				return true
			}
		}
	}
	return false
}

// configChangeParams 是 vm.config.update 任务的参数。
type configChangeParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// Changes 是**仅包含变化项**的字段集合（差异提交）。
	Changes map[string]any `json:"changes"`
	// ObservedStatus 是受理时探测到的状态，仅作排障线索。
	ObservedStatus string `json:"observed_status"`
}
