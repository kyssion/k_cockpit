package vm

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// rescueParams 是 vm.rescue.enter / vm.rescue.exit 的参数。
type rescueParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// Snapshot 是进入救援前的配置快照（JSON 文本）。
	//
	// enter 时由控制面写入并落库；exit 时从库里取出、随指令发给节点，
	// 并在成功后写回 vm 记录。**随任务持久化而不是只放内存**：进入与退出
	// 之间可能隔着几小时甚至几天，那时发起进入的人早就关掉页面了。
	Snapshot       string `json:"snapshot"`
	ObservedStatus string `json:"observed_status"`
}

// EnterRescueExecutor 执行 vm.rescue.enter 任务。
type EnterRescueExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewEnterRescueExecutor 构造进入救援执行器。
func NewEnterRescueExecutor(db *gorm.DB, client agent.Client) *EnterRescueExecutor {
	return &EnterRescueExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *EnterRescueExecutor) Type() string { return model.TaskVMRescueEnter }

// Run 下发救援启动并记录快照。
//
// 顺序：**先让节点改好配置、再写控制面状态**。反过来的话，节点操作失败时
// 虚拟机记录会显示「救援中」，而它其实还从系统盘引导——用户会按救援环境的
// 预期去操作一台正常的机器。
func (e *EnterRescueExecutor) Run(ctx context.Context, t *model.Task) error {
	var p rescueParams
	if !decodeRescueParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMRescueEnter,
		NodeID: *t.NodeID,
		Target: p.VMName,
		// 节点需要知道**原配置是什么**才谈得上「改」：它要按救援档案调整
		// 引导顺序与盘型，而调整的依据是当前值。
		Params: map[string]any{"snapshot": p.Snapshot},
	})
	if err != nil {
		return api.Unavailable("节点不可达，救援指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	now := time.Now()
	if err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).
		Updates(map[string]any{
			"rescue_active": true,
			"rescue_config": p.Snapshot,
			"rescue_since":  now,
		}).Error; err != nil {
		log.Printf("[vm] 记录救援状态失败 vm=%d: %v", p.VMID, err)
		// 不返回错误：宿主机上的配置已经改好了，此时报失败会让用户以为
		// 救援没生效而去重试，而重试会被「已在救援模式」拒绝，更困惑。
		// 状态由对账收敛，日志留痕。
	}

	log.Printf("[vm] 已进入救援模式 id=%d name=%s task=%d", p.VMID, p.VMName, t.ID)
	return nil
}

// ExitRescueExecutor 执行 vm.rescue.exit 任务：还原配置并退出救援。
type ExitRescueExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExitRescueExecutor 构造退出救援执行器。
func NewExitRescueExecutor(db *gorm.DB, client agent.Client) *ExitRescueExecutor {
	return &ExitRescueExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ExitRescueExecutor) Type() string { return model.TaskVMRescueExit }

// Run 下发还原指令并清除救援状态。
func (e *ExitRescueExecutor) Run(ctx context.Context, t *model.Task) error {
	var p rescueParams
	if !decodeRescueParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMRescueExit,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{"snapshot": p.Snapshot},
	})
	if err != nil {
		return api.Unavailable("节点不可达，还原指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	updates := map[string]any{
		"rescue_active": false,
		// 快照与时刻一起清掉：留着一份不再对应的快照，下次进入救援时
		// 会被覆盖，而如果那次进入失败，这份旧的就会被当成「本次的原配置」
		// 去还原——那会还原到一个更久远的状态。
		"rescue_config": nil,
		"rescue_since":  nil,
	}

	// 把快照里的字段**写回记录**：宿主机上已经还原了，控制面必须跟着回到
	// 一致的状态。只清救援标记而不还原字段，会让控制面显示「引导顺序 CD-ROM」，
	// 而实际已经恢复成磁盘引导——两边分叉，且下一次编辑会以错误的前提提交。
	var snap rescueSnapshot
	if err := json.Unmarshal([]byte(p.Snapshot), &snap); err == nil {
		if snap.BootOrder != "" {
			updates["boot_order"] = snap.BootOrder
		}
		if snap.MachineType != "" {
			updates["machine_type"] = snap.MachineType
		}
		if snap.DisplayDevice != "" {
			updates["display_device"] = snap.DisplayDevice
		}
		if snap.Firmware != "" {
			updates["firmware"] = snap.Firmware
		}
		updates["secure_boot"] = snap.SecureBoot
	} else {
		log.Printf("[vm] 解析救援快照失败（将只清除救援标记）vm=%d: %v", p.VMID, err)
	}

	if err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).Updates(updates).Error; err != nil {
		log.Printf("[vm] 清除救援状态失败 vm=%d: %v", p.VMID, err)
	}

	log.Printf("[vm] 已退出救援模式 id=%d name=%s task=%d", p.VMID, p.VMName, t.ID)
	return nil
}

// decodeRescueParams 解析任务参数。
func decodeRescueParams(t *model.Task, dst *rescueParams) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[vm] 解析救援参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}
