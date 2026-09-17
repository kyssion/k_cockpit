package vm

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// guestParams 是 vm.guest 任务的参数。
type guestParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	Action string `json:"action"`

	Username string `json:"username,omitempty"`
	// Password 只在入队时写入，**执行后立即清除**（见 scrubPassword）。
	Password string `json:"password,omitempty"`

	DiskID string `json:"disk_id,omitempty"`
	DiskGB int    `json:"disk_gb,omitempty"`

	ObservedStatus string `json:"observed_status,omitempty"`
}

// GuestExecutor 执行 vm.guest 任务（F-2-10）。
type GuestExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewGuestExecutor 构造来宾自动化执行器。
func NewGuestExecutor(db *gorm.DB, client agent.Client) *GuestExecutor {
	return &GuestExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *GuestExecutor) Type() string { return model.TaskVMGuest }

// Run 下发来宾自动化指令。
//
// **执行后立即清除参数里的密码**（R-009：口令不落库）。任务参数是持久化的，
// 不改的话它会一直留在 task.params 里——而那个表是运维随时会翻的。
//
// 清除放在 defers 里而不是成功路径上：**失败路径同样要清**。恰恰是失败时
// 用户最可能把参数贴出来求助。
func (e *GuestExecutor) Run(ctx context.Context, t *model.Task) error {
	defer e.scrubPassword(ctx, t.ID)

	var p guestParams
	if !decodeGuest(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	params := map[string]any{"action": p.Action}
	if p.Username != "" {
		params["username"] = p.Username
	}
	if p.Password != "" {
		params["password"] = p.Password
	}
	if p.DiskID != "" {
		params["disk_id"] = p.DiskID
	}
	if p.DiskGB > 0 {
		params["disk_gb"] = p.DiskGB
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMGuest,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: params,
	})
	if err != nil {
		return api.Unavailable("节点不可达，来宾操作未送达")
	}
	if !result.Success {
		// 节点侧的失败原因更准（agent 没响应、密码策略拒绝、文件系统不支持
		// 在线扩容），原样带出。
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.GuestDataKey].(agent.GuestInfo)
	if info.Message != "" {
		log.Printf("[vm] 来宾操作完成 vm=%s action=%s guest_agent=%v: %s",
			p.VMName, p.Action, info.GuestAgentUsed, info.Message)
	} else {
		log.Printf("[vm] 来宾操作完成 vm=%s action=%s task=%d", p.VMName, p.Action, t.ID)
	}
	return nil
}

// scrubPassword 把任务参数里的密码抹掉，只留下其余字段。
//
// 重写整个 params 而不是「只删 password 键」：JSON 文本没有办法做局部删除，
// 而留下一个被置空的 password 字段同样会让人以为「这里曾经有个密码」。
func (e *GuestExecutor) scrubPassword(ctx context.Context, taskID int64) {
	var row model.Task
	if err := e.db.WithContext(ctx).Select("id", "params").
		Where("id = ?", taskID).First(&row).Error; err != nil {
		log.Printf("[vm] 清除任务密码失败（读取）task=%d: %v", taskID, err)
		return
	}
	if row.Params == nil || *row.Params == "" {
		return
	}

	var p guestParams
	if err := json.Unmarshal([]byte(*row.Params), &p); err != nil {
		// 解析不了就不动它：把一份读不懂的参数改写成另一份读不懂的，只会
		// 让排查更难。日志留痕即可。
		log.Printf("[vm] 清除任务密码失败（解析）task=%d: %v", taskID, err)
		return
	}
	if p.Password == "" {
		return
	}
	p.Password = ""

	scrubbed, err := json.Marshal(p)
	if err != nil {
		log.Printf("[vm] 清除任务密码失败（序列化）task=%d: %v", taskID, err)
		return
	}
	if err := e.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ?", taskID).Update("params", string(scrubbed)).Error; err != nil {
		log.Printf("[vm] 清除任务密码失败（写入）task=%d: %v", taskID, err)
	}
}

// decodeGuest 解析任务参数。
func decodeGuest(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[vm] 解析来宾操作参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}
