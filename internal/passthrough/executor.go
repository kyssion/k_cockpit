package passthrough

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// Executor 执行直通设备的挂载与卸载。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *Executor) Type() string { return model.TaskPassthroughChange }

// Run 下发。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[passthrough] 解析任务参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpHostPCIBind,
		NodeID: nodeID,
		Target: strOf(p["pci_address"]),
		Params: map[string]any{
			"action":      "attach_or_detach",
			"attach":      strOf(p["action"]) == "attach",
			"vm_name":     strOf(p["vm_name"]),
			"pci_address": strOf(p["pci_address"]),
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，直通设备未变更")
	}
	if !result.Success {
		// **失败保留记录。**
		//
		// 与目录共享不同：那里的失败意味着"什么都没挂上"，删掉记录是对的；
		// 而这里**设备绑定这一步可能已经成功**（bind 与 hostdev 是两步），
		// 只是把设备写进域配置失败了。删掉记录会让那台机器从此挂着一块
		// 谁也看不见的直通设备，而用户以为自己卸掉了。
		return api.ValidationFailed(result.Message)
	}
	return nil
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}
