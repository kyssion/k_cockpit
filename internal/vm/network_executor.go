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
)

// InterfaceChangeExecutor 执行 vm.interface.change 任务。
type InterfaceChangeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewInterfaceChangeExecutor 构造网卡变更执行器。
func NewInterfaceChangeExecutor(db *gorm.DB, client agent.Client) *InterfaceChangeExecutor {
	return &InterfaceChangeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *InterfaceChangeExecutor) Type() string { return model.TaskVMInterfaceChange }

// Run 下发网卡变更并把下发时间写回记录。
func (e *InterfaceChangeExecutor) Run(ctx context.Context, t *model.Task) error {
	var p netChangeParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpVMInterfaceChange, NodeID: *t.NodeID,
		Target: p.VMName, Params: p.Spec,
	})
	if err != nil {
		return api.Unavailable("节点不可达，网卡变更未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 只有成功的下发才更新 last_applied_at——它表示「这份配置已经在节点上
	// 生效了」。失败时留空/保留旧值，界面据此显示「未生效」。
	//
	// 删除动作的记录已经被移除，更新会影响 0 行，这是正常的。
	if !e.markApplied(ctx, &model.VMInterface{}, p) {
		log.Printf("[vm] 网卡记录已不存在（删除动作），跳过回写 spec=%v", p.Spec["interface_id"])
	}

	log.Printf("[vm] 网卡变更完成 vm=%s action=%v task=%d", p.VMName, p.Spec["action"], t.ID)
	return nil
}

// markApplied 把下发时间写回对应记录；记录不存在时返回 false。
func (e *InterfaceChangeExecutor) markApplied(
	ctx context.Context, dst any, p netChangeParams,
) bool {
	id, ok := specInt64(p.Spec, "interface_id")
	if !ok || id == 0 {
		return false
	}
	res := e.db.WithContext(ctx).Model(dst).
		Where("id = ?", id).
		Updates(map[string]any{"last_applied_at": time.Now(), "updated_at": time.Now()})
	if res.Error != nil {
		log.Printf("[vm] 回写网卡下发时间失败 id=%d: %v", id, res.Error)
		return false
	}
	return res.RowsAffected > 0
}

// StaticIPChangeExecutor 执行 vm.staticip.change 任务。
type StaticIPChangeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewStaticIPChangeExecutor 构造静态地址变更执行器。
func NewStaticIPChangeExecutor(db *gorm.DB, client agent.Client) *StaticIPChangeExecutor {
	return &StaticIPChangeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *StaticIPChangeExecutor) Type() string { return model.TaskVMStaticIPChange }

// Run 下发静态地址变更。
func (e *StaticIPChangeExecutor) Run(ctx context.Context, t *model.Task) error {
	var p netChangeParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpVMStaticIPChange, NodeID: *t.NodeID,
		Target: p.VMName, Params: p.Spec,
	})
	if err != nil {
		return api.Unavailable("节点不可达，地址变更未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 解绑时记录已被删除，更新影响 0 行是正常的。
	if id, ok := specInt64(p.Spec, "static_ip_id"); ok && id > 0 {
		res := e.db.WithContext(ctx).Model(&model.StaticIP{}).
			Where("id = ?", id).
			Updates(map[string]any{"applied_at": time.Now(), "updated_at": time.Now()})
		if res.Error != nil {
			log.Printf("[vm] 回写地址下发时间失败 id=%d: %v", id, res.Error)
		}
	}

	log.Printf("[vm] 静态地址变更完成 vm=%s action=%v task=%d", p.VMName, p.Spec["action"], t.ID)
	return nil
}

// PortForwardChangeExecutor 执行 vm.portforward.change 任务。
type PortForwardChangeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPortForwardChangeExecutor 构造端口转发变更执行器。
func NewPortForwardChangeExecutor(db *gorm.DB, client agent.Client) *PortForwardChangeExecutor {
	return &PortForwardChangeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PortForwardChangeExecutor) Type() string { return model.TaskVMPortForwardChange }

// Run 下发端口转发变更。
func (e *PortForwardChangeExecutor) Run(ctx context.Context, t *model.Task) error {
	var p netChangeParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpVMPortForwardChange, NodeID: *t.NodeID,
		Target: p.VMName, Params: p.Spec,
	})
	if err != nil {
		return api.Unavailable("节点不可达，端口转发变更未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 按 (协议, 宿主机端口) 定位记录而不是按 id：删除动作的记录已经消失，
	// 而新增动作在受理时就已经写好了那一行。用业务键定位让两种动作共用
	// 同一段回写逻辑。
	protocol, _ := p.Spec["protocol"].(string)
	hostPort, _ := specInt64(p.Spec, "host_port")
	if protocol != "" && hostPort > 0 {
		res := e.db.WithContext(ctx).Model(&model.PortForward{}).
			Where("protocol = ? AND host_port = ?", protocol, hostPort).
			Updates(map[string]any{"last_applied_at": time.Now(), "updated_at": time.Now()})
		if res.Error != nil {
			log.Printf("[vm] 回写端口转发下发时间失败 %s/%d: %v", protocol, hostPort, res.Error)
		}
	}

	log.Printf("[vm] 端口转发变更完成 vm=%s action=%v task=%d", p.VMName, p.Spec["action"], t.ID)
	return nil
}

// netChangeParams 是三种网络变更任务共用的参数。
//
// 三者形状完全一致（动作 + 完整的目标状态），因此共用一个结构：
// 拆成三个只会得到三份几乎相同的代码，而它们真正的差异在 Spec 里。
type netChangeParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// Spec 是下发给节点的**完整目标状态**，不是增量补丁。
	//
	// 用完整状态而不是 diff：节点侧不需要知道「之前是什么」，只负责把当前
	// 配置对齐到控制面描述的样子——这让重试天然幂等。
	Spec map[string]any `json:"-"`
}

// UnmarshalJSON 让 Spec 直接取整个参数对象。
//
// 网络变更的下发描述字段是随资源类型变化的（网卡有 mac、端口转发有 protocol），
// 因此不在 Go 结构体里逐个声明，而是整体透传。用自定义反序列化而不是
// map[string]any 直传，是为了让 VMID / VMName 这两个**所有类型都有**的字段
// 仍能按名字取到，而不必处处在 map 里翻。
func (p *netChangeParams) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.Spec = raw
	if v, ok := raw["vm_id"].(float64); ok {
		p.VMID = int64(v)
	}
	if s, ok := raw["vm_name"].(string); ok {
		p.VMName = s
	}
	return nil
}

// specInt64 从下发描述里取一个整数值。
//
// JSON 反序列化后的数字是 float64，直接断言 int64 会失败并静默得到 0——
// 而 0 会让回写变成「更新 id=0 的记录」，既不报错也不生效。
func specInt64(spec map[string]any, key string) (int64, bool) {
	switch v := spec[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}
