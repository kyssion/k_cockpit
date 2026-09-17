package publicip

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

// changeParams 是 public_ip.change 任务的参数。
type changeParams struct {
	PublicIPID int64  `json:"public_ip_id"`
	IP         string `json:"ip"`
	// Action 取值 bind / unbind / migrate。
	Action string `json:"action"`
	Mode   string `json:"mode"`
	VMID   int64  `json:"vm_id,omitempty"`
	VMName string `json:"vm_name,omitempty"`
}

// ChangeExecutor 执行公网地址变更（F-4-06）。
type ChangeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewChangeExecutor 构造执行器。
func NewChangeExecutor(db *gorm.DB, client agent.Client) *ChangeExecutor {
	return &ChangeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ChangeExecutor) Type() string { return model.TaskPublicIPChange }

// Run 下发变更并在成功后同步控制面记录。
//
// 顺序是**先让节点成功、再改记录**。反过来的话，节点失败时控制面会认为
// 地址已经指向了新目标，而宿主机上那条规则根本不存在——用户按「已经生效」
// 的前提去排查，方向从一开始就是错的。
//
// 迁移（migrate）的失败处理尤其要紧：节点侧按「先撤旧、再加新」执行，
// 中途失败时旧规则仍然有效。因此这里**什么都不改**——控制面继续显示
// 原来的持有者，与宿主机的实际状态一致。
func (e *ChangeExecutor) Run(ctx context.Context, t *model.Task) error {
	var p changeParams
	if !decode(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	params := map[string]any{
		"action": p.Action,
		"mode":   p.Mode,
		"ip":     p.IP,
	}
	if p.VMName != "" {
		params["vm_name"] = p.VMName
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpPublicIPChange,
		NodeID: *t.NodeID,
		Target: p.IP,
		Params: params,
	})
	if err != nil {
		return api.Unavailable("节点不可达，地址变更未送达")
	}
	if !result.Success {
		// 节点侧的失败原因更准（地址被占用、上游路由不通），原样带出。
		return api.ValidationFailed(result.Message)
	}

	// 绑定与迁移都要在**一个事务**里完成「释放旧绑定 + 建立新绑定」。
	//
	// 分开写的话，中途失败会得到「旧绑定已释放、新绑定没建起来」的悬空
	// 状态——地址此时不指向任何地方，而控制面认为迁移成功了。那是故障
	// 转移场景下最不能出现的结果：外部访问已经断了，用户却以为一切正常。
	switch p.Action {
	case "bind", "migrate":
		if err := e.bindTx(ctx, &p); err != nil {
			log.Printf("[publicip] 同步绑定记录失败 ip=%s: %v", p.IP, err)
			return api.Internal()
		}
	case "unbind":
		if err := e.unbindTx(ctx, &p); err != nil {
			log.Printf("[publicip] 同步解绑记录失败 ip=%s: %v", p.IP, err)
			return api.Internal()
		}
	default:
		log.Printf("[publicip] 未知的地址动作 %q task=%d", p.Action, t.ID)
		return api.Internal()
	}

	log.Printf("[publicip] 地址变更完成 ip=%s action=%s vm=%s task=%d",
		p.IP, p.Action, p.VMName, t.ID)
	return nil
}

// bindTx 在一个事务里释放旧绑定并建立新绑定。
func (e *ChangeExecutor) bindTx(ctx context.Context, p *changeParams) error {
	now := time.Now()
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 释放该地址上所有仍然有效的绑定（正常情况下至多一条）。
		//
		// 用 update 而不是 delete：绑定记录要**留着**并标记释放时间——
		// 事后追查「这个地址在某个时间点指向谁」时，唯一能回答的就是
		// 这条历史。删掉它，那段历史就无从查起。
		if err := tx.Model(&model.PublicIPBinding{}).
			Where("public_ip_id = ? AND released_at IS NULL", p.PublicIPID).
			Update("released_at", now).Error; err != nil {
			return err
		}

		vmID := p.VMID
		binding := model.PublicIPBinding{
			PublicIPID:    p.PublicIPID,
			NodeID:        nodeIDOfBinding(tx, p.PublicIPID),
			VMID:          &vmID,
			Mode:          p.Mode,
			RuntimeStatus: model.BindingActive,
			BoundAt:       &now,
		}
		if err := tx.Create(&binding).Error; err != nil {
			return err
		}

		return tx.Model(&model.PublicIP{}).Where("id = ?", p.PublicIPID).
			Update("status", model.PublicIPBound).Error
	})
}

// unbindTx 释放绑定并把地址退回可用池。
func (e *ChangeExecutor) unbindTx(ctx context.Context, p *changeParams) error {
	now := time.Now()
	return e.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.PublicIPBinding{}).
			Where("public_ip_id = ? AND released_at IS NULL", p.PublicIPID).
			Update("released_at", now).Error; err != nil {
			return err
		}
		return tx.Model(&model.PublicIP{}).Where("id = ?", p.PublicIPID).
			Update("status", model.PublicIPAvailable).Error
	})
}

// nodeIDOfBinding 取地址所属节点。
//
// 在事务里查一次而不是让它随任务参数传来：节点号是地址的固有属性，
// 参数里重复一份只会在两者不一致时制造困惑。
func nodeIDOfBinding(tx *gorm.DB, publicIPID int64) int64 {
	var ip model.PublicIP
	if err := tx.Select("node_id").Where("id = ?", publicIPID).First(&ip).Error; err != nil {
		log.Printf("[publicip] 查询地址节点失败 id=%d: %v", publicIPID, err)
	}
	return ip.NodeID
}

func decode(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[publicip] 解析任务参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}
