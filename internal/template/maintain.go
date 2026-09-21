package template

import (
	"context"
	"log"
	"time"

	"gorm.io/gorm"
	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// 派生链维护的四种动作。它们改的是**同一条链**，因此共用一个任务类型，
// 由 Action 区分——下发时再各自映射到不同的 agent 操作。
const (
	maintainRebase        = "rebase"
	maintainFlatten       = "flatten"
	maintainPromoteChild  = "promote_child"
	maintainPromoteDelete = "promote_delete"
)

// MaintainRequest 是一次派生链维护。
type MaintainRequest struct {
	Action string
	// ChildID 仅在 promote_child 时有意义：要提升的那个子模板。
	ChildID int64
	// Acknowledge 表示用户已知悉影响（拉平/重挂会改写数据、影响下游）。
	Acknowledge bool
}

// Maintain 受理一次派生链维护。
//
// 为什么要这些动作：链式克隆里，中间一代一旦被直接删掉，下游全部失效，而
// 节点上的表现是"虚拟机还在跑，读某个块时才报错"——那种故障要等到数据被
// 访问才暴露，排查成本极高。promote_delete 让"删中间一代"成为一个**安全的
// 显式动作**（先把下游改挂到上级，再删）。
func (s *Service) Maintain(
	ctx context.Context, id int64, req MaintainRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	switch req.Action {
	case maintainRebase, maintainFlatten, maintainPromoteDelete, maintainPromoteChild:
	default:
		return nil, api.InvalidParameter(
			"不支持的维护动作，可选 rebase / flatten / promote_child / promote_delete")
	}
	if req.Action == maintainPromoteChild && req.ChildID <= 0 {
		return nil, api.InvalidParameter("必须指定要提升的子模板")
	}
	if !req.Acknowledge {
		return nil, api.Conflict(acknowledgeReason(req.Action))
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskTemplateMaintain,
		NodeID:       tpl.NodeID,
		ResourceType: "template",
		ResourceID:   tpl.ID,
		ResourceName: tpl.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: maintainParams{
			Action:     req.Action,
			TemplateID: tpl.ID,
			ChildID:    req.ChildID,
			DiskPath:   tpl.DiskPathOf(),
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, v.UserID, operatorName, clientIP, "template.maintain", tpl.NodeID, tpl.Name, t.ID)
	return t, nil
}

// acknowledgeReason 给出"为什么需要确认"的具体理由。
//
// 写具体而不是写"操作有风险"：用户要回答的是"我现在能不能做"，而那取决于
// 这次到底会改写多少数据、影响几台机器。
func acknowledgeReason(action string) string {
	switch action {
	case maintainRebase:
		return "该操作要把差异写回上级并重写这条派生链，期间相关模板不可用；确认请勾选"
	case maintainFlatten:
		return "在线拉平不中断正在使用这块盘的虚拟机，但会持续占用 I/O 直到完成；确认请勾选"
	case maintainPromoteChild:
		return "提升后该子模板不再依赖当前模板；确认请勾选"
	case maintainPromoteDelete:
		return "删除这一代前会先把它的下游模板改挂到上级，期间相关模板不可用；确认请勾选"
	default:
		return "该操作会影响派生链上的其它模板；确认请勾选"
	}
}

type maintainParams struct {
	Action     string `json:"action"`
	TemplateID int64  `json:"template_id"`
	ChildID    int64  `json:"child_id"`
	DiskPath   string `json:"disk_path"`
}

// MaintainExecutor 执行派生链维护。
type MaintainExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewMaintainExecutor 构造执行器。
func NewMaintainExecutor(db *gorm.DB, client agent.Client) *MaintainExecutor {
	return &MaintainExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *MaintainExecutor) Type() string { return model.TaskTemplateMaintain }

// Run 下发动作，成功后再改控制面的父子关系。
//
// 顺序与别处一致：**先让节点上的操作成功，再写控制面记录**。反过来会在节点
// 失败时留下一条"已经改挂了、实际没改"的记录——那比不改更糟。
func (e *MaintainExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p maintainParams
	if !decode(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	kind := agent.OpTemplateRebase
	switch p.Action {
	case maintainFlatten:
		kind = agent.OpTemplateFlatten
	case maintainPromoteChild:
		kind = agent.OpTemplatePromoteChild
	case maintainPromoteDelete:
		kind = agent.OpTemplatePromoteDelete
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   kind,
		NodeID: *t.NodeID,
		Target: p.DiskPath,
		Params: map[string]any{"template_id": p.TemplateID, "child_id": p.ChildID},
	})
	if err != nil {
		return api.Unavailable("节点不可达，派生链未改动")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	now := time.Now()
	switch p.Action {
	case maintainPromoteChild:
		// 子模板改挂到当前模板的父上（没有父就变成独立的一代）。
		var cur model.Template
		if err := e.db.WithContext(ctx).Select("id", "parent_id").
			Where("id = ?", p.TemplateID).First(&cur).Error; err != nil {
			log.Printf("[template] 读取模板失败 id=%d: %v", p.TemplateID, err)
			return api.Internal()
		}
		if err := e.db.WithContext(ctx).Model(&model.Template{}).
			Where("id = ?", p.ChildID).
			Updates(map[string]any{"parent_id": cur.ParentID, "updated_at": now}).Error; err != nil {
			log.Printf("[template] 提升子模板失败 id=%d: %v", p.ChildID, err)
			return api.Internal()
		}

	case maintainPromoteDelete:
		var cur model.Template
		if err := e.db.WithContext(ctx).Select("id", "parent_id", "family_id").
			Where("id = ?", p.TemplateID).First(&cur).Error; err != nil {
			log.Printf("[template] 读取模板失败 id=%d: %v", p.TemplateID, err)
			return api.Internal()
		}
		// 下游先改挂，再删这一代。顺序反了会出现"父子都指向一个已删除的模板"。
		if err := e.db.WithContext(ctx).Model(&model.Template{}).
			Where("parent_id = ?", p.TemplateID).
			Updates(map[string]any{"parent_id": cur.ParentID, "updated_at": now}).Error; err != nil {
			log.Printf("[template] 改挂下游模板失败 id=%d: %v", p.TemplateID, err)
			return api.Internal()
		}
		if err := e.db.WithContext(ctx).Model(&model.Template{}).
			Where("id = ?", p.TemplateID).
			Updates(map[string]any{"deleted_at": now, "updated_at": now}).Error; err != nil {
			log.Printf("[template] 删除模板失败 id=%d: %v", p.TemplateID, err)
			return api.Internal()
		}
	}
	return nil
}

// FamilyTree 返回同一族的全部版本，供界面的族树使用。
//
// 它直接复用 Family：那个接口已经按 version 排好序，并且 View 里带着
// parent_id，前端据此就能拼出层级——为树视图再做一个接口只会多一份
// 需要维护的排序口径。
func (s *Service) FamilyTree(ctx context.Context, id int64, v authz.Viewer) ([]View, error) {
	return s.Family(ctx, id, v)
}
