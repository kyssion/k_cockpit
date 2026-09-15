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

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMCreate,
		NodeID: p.NodeID,
		Target: p.Name,
		Params: map[string]any{
			"vcpu":      p.VCPU,
			"memory_mb": p.MemoryMB,
			"disk_gb":   p.DiskGB,
		},
	})
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
