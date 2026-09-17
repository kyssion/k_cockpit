package vm

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// createParams 是 vm.create 任务的参数。
//
// 它刻意与 vm.CreateRequest 分开：前者是**已入队并持久化到 task.params 的
// 任务参数**，后者是接口请求。两者字段相近但生命周期不同——共用结构会让
// 「接口加一个字段」意外影响历史任务的反序列化。
type createParams struct {
	Name      string `json:"name"`
	NodeID    int64  `json:"node_id"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	Remark    string `json:"remark"`
	GroupName string `json:"group_name"`
	OwnerID   int64  `json:"owner_id"`

	// --- 从模板克隆（f-3-02）；TemplateID 为零表示从零安装 ---

	TemplateID int64  `json:"template_id,omitempty"`
	CloneMode  string `json:"clone_mode,omitempty"`
	// TemplateDiskPath 是模板盘在宿主机上的路径，随任务持久化。
	//
	// 不在这里回查模板表：任务可能排很久才执行，那时模板已被删除或改名，
	// 回查会得到空值或另一份路径。**入队那一刻的路径才是这次要用的**。
	TemplateDiskPath string `json:"template_disk_path,omitempty"`
}

// CreateExecutor 执行 vm.create 任务。
type CreateExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewCreateExecutor 构造创建执行器。
func NewCreateExecutor(db *gorm.DB, client agent.Client) *CreateExecutor {
	return &CreateExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *CreateExecutor) Type() string { return model.TaskVMCreate }

// Run 下发创建指令并写入投影。
//
// 顺序很重要：**先让虚拟化层创建成功，再写投影**。反过来的话，创建失败时
// 会留下一条并不存在的虚拟机记录——用户看到它、点进去操作、然后收到
// 「不存在」，而原因早已无从查起。
func (e *CreateExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p createParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析创建参数失败: %v", err)
		return api.Internal()
	}

	// 有模板就走克隆，没有就从零安装。
	//
	// 两者在控制面是**同一个任务类型、同一套受理逻辑**（对用户来说「从模板
	// 建一台机器」与「新建一台机器」是同一件事），只在最后下发时分开——
	// 节点侧也只需要实现「给我一块盘，从它派生」，而不必理解模板的概念。
	op := agent.Operation{
		Kind:   agent.OpVMCreate,
		NodeID: p.NodeID,
		Target: p.Name,
		Params: map[string]any{
			"vcpu":      p.VCPU,
			"memory_mb": p.MemoryMB,
			"disk_gb":   p.DiskGB,
		},
	}
	if p.TemplateID > 0 {
		op.Kind = agent.OpVMClone
		op.Params["template_id"] = p.TemplateID
		op.Params["clone_mode"] = p.CloneMode
		op.Params["template_disk_path"] = p.TemplateDiskPath
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, op)
	if err != nil {
		// 指令未送达与执行失败是两回事，但对用户而言都需要一个可操作的说法。
		return api.Unavailable("节点不可达，创建指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	now := time.Now()
	vm := model.VM{
		NodeID:    p.NodeID,
		Name:      p.Name,
		VCPU:      p.VCPU,
		MemoryMB:  p.MemoryMB,
		DiskGB:    p.DiskGB,
		OwnerID:   optID(p.OwnerID),
		Remark:    optStr(p.Remark),
		GroupName: optStr(p.GroupName),

		// 状态以 agent 返回为准；拿不到时记为 unknown 而**不是**猜一个
		// running——猜错会让界面显示「运行中」，而用户点关机才发现它没起来。
		Status:       model.VMStatusUnknown,
		Present:      true,
		LastSyncedAt: &now,
	}
	if p.TemplateID > 0 {
		vm.TemplateID = &p.TemplateID
		vm.CloneMode = p.CloneMode
		if vm.CloneMode == "" {
			vm.CloneMode = model.CloneFull
		}
		// 链式克隆的父盘路径由**节点返回**，不按模板路径推算：真实实现
		// 可能为了性能把模板盘放到别处（比如 SSD 缓存层），由节点说了算
		// 才不会对不上。它是排查「克隆机起不来」时第一个要看的东西。
		if info, ok := result.Data[agent.CloneDataKey].(agent.CloneInfo); ok && info.BackingPath != "" {
			vm.BackingPath = &info.BackingPath
		}
	} else {
		vm.CloneMode = model.CloneFull
	}

	if uuid, ok := result.Data["uuid"].(string); ok && uuid != "" {
		vm.UUID = &uuid
	}
	if status, ok := result.Data["status"].(string); ok && status != "" {
		vm.Status = status
	}

	if err := e.db.WithContext(ctx).Create(&vm).Error; err != nil {
		// 同名冲突是最常见的失败：给出可操作的原因，而不是笼统的 500。
		if isDuplicateKey(err) {
			return api.Conflict("同名虚拟机已存在")
		}
		log.Printf("[vm] 写入虚拟机投影失败: %v", err)
		return api.Internal()
	}

	log.Printf("[vm] 已创建虚拟机 id=%d name=%s node=%d task=%d", vm.ID, vm.Name, vm.NodeID, t.ID)
	return nil
}

// powerParams 是 vm.power 任务的参数。
type powerParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	Action string `json:"action"`
	// ObservedStatus 是受理时探测到的状态，用于事后区分「受理时探测结果与
	// 投影不一致」与「执行时状态已变」两种情况。它**不参与执行判定**：
	// 判定已在受理时完成，这里只作为排障线索。
	ObservedStatus string `json:"observed_status"`
}

// PowerExecutor 执行 vm.power 任务。
type PowerExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPowerExecutor 构造电源执行器。
func NewPowerExecutor(db *gorm.DB, client agent.Client) *PowerExecutor {
	return &PowerExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PowerExecutor) Type() string { return model.TaskVMPower }

// Run 下发电源指令并回写投影状态。
//
// 这里**不重复探测**：状态合法性已在受理时校验（Service.Power），而任务的
// 资源锁保证同一虚拟机的操作串行执行，不会插入其他操作。再次探测只会把
// 竞态窗口从「受理到执行」扩大到「两次探测之间」，得不偿失。
func (e *PowerExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p powerParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析电源参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	action := PowerAction(p.Action)

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   action.Op(),
		NodeID: *t.NodeID,
		Target: p.VMName,
	})
	if err != nil {
		// 指令未送达：不能断言操作失败，但用户需要一个可操作的说法。
		return api.Unavailable("节点不可达，电源指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 状态以 agent 返回为准；它没返回时用动作的期望状态。
	//
	// 记 expected 而**不是**沿用投影值：投影是操作前的旧值，沿用它等于
	// 让界面在操作成功后仍显示旧状态，看起来像操作没生效。
	status := targetStatus[action]
	if reported, ok := result.Data[agent.StatusDataKey].(string); ok && reported != "" {
		status = reported
	}
	e.updateStatus(ctx, p.VMID, status)

	log.Printf("[vm] 电源操作完成 id=%d action=%s status=%s task=%d", p.VMID, action, status, t.ID)
	return nil
}

// updateStatus 回写投影状态。
//
// 失败时**不返回错误**：宿主侧操作已经成功，只是本地投影没写上。此时让任务
// 报失败会误导用户去重试一个已经生效的操作（重试还会被状态机拒绝，更困惑）。
// 投影是缓存，下一次对账会收敛；日志里留痕即可。
func (e *PowerExecutor) updateStatus(ctx context.Context, vmID int64, status string) {
	now := time.Now()
	err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vmID).
		Updates(map[string]any{"status": status, "last_synced_at": now}).Error
	if err != nil {
		log.Printf("[vm] 回写投影状态失败 id=%d status=%s: %v", vmID, status, err)
	}
}

// deleteParams 是 vm.delete 任务的参数。
type deleteParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// DiskAction 取值 delete / keep，由用户在界面上显式选择（R-009）。
	DiskAction     string `json:"disk_action"`
	ObservedStatus string `json:"observed_status"`
}

// DeleteExecutor 执行 vm.delete 任务。
type DeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDeleteExecutor 构造删除执行器。
func NewDeleteExecutor(db *gorm.DB, client agent.Client) *DeleteExecutor {
	return &DeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DeleteExecutor) Type() string { return model.TaskVMDelete }

// Run 下发删除指令并把投影标记为「已不在虚拟化层」。
func (e *DeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p deleteParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析删除参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMDelete,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{"disk_action": p.DiskAction},
	})
	if err != nil {
		return api.Unavailable("节点不可达，删除指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// **不物理删除记录**：审计流水与历史任务都会引用它，删掉会让这些引用
	// 悬空，事后追查「谁在什么时候删了哪台机器」就无从谈起。标记 present
	// 为 false 后列表不再显示，但记录保留（f-2-01 §5.1）。
	now := time.Now()
	err = e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).
		Updates(map[string]any{
			"present":        false,
			"status":         model.VMStatusUnknown,
			"last_synced_at": now,
		}).Error
	if err != nil {
		log.Printf("[vm] 标记虚拟机已删除失败 id=%d: %v", p.VMID, err)
	}

	log.Printf("[vm] 已删除虚拟机 id=%d name=%s disk_action=%s task=%d",
		p.VMID, p.VMName, p.DiskAction, t.ID)
	return nil
}

// isDuplicateKey 判断错误是否为唯一约束冲突。
func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

func optID(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
