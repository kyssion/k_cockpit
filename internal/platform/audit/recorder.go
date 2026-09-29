// Package audit 写入审计流水（F-1-12）。
//
// audit_log 是只增不改的流水表（DATA_MODEL §4.14）。两条约定：
//
//   - **写入失败不影响主流程**：审计不应让业务操作失败；
//   - 但失败**必须留下日志**，否则会出现「操作成功却无审计」的静默缺口——
//     这类缺口在事后追溯时无法补救，比操作本身失败更严重。
package audit

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// Recorder 写入审计记录。
type Recorder struct {
	db *gorm.DB
}

// NewRecorder 构造写入器。
func NewRecorder(db *gorm.DB) *Recorder {
	return &Recorder{db: db}
}

// Entry 描述一次审计事件。
//
// 全部字段都是值类型：零值表示「不适用」，由写入器转为 NULL。
// 资源名与操作人名以快照形式记录，保证历史审计在改名或删除后仍可还原。
type Entry struct {
	OperatorID   int64  // 0 表示系统动作
	OperatorName string // 快照
	Source       string // 见 model.Source*；空则记为 web
	NodeID       int64
	ResourceType string // 必填
	ResourceID   int64
	ResourceName string // 快照
	Action       string // 必填，如 user.login、session.revoke
	Params       any    // 调用方负责脱敏；不得包含密码、令牌
	BeforeState  any
	AfterState   any
	Success      bool
	Error        string
	ClientIP     string
}

// Record 写入一条审计记录。
//
// 不返回错误：审计失败不应中断业务，但会在日志中留下明确标记。
func (r *Recorder) Record(ctx context.Context, e Entry) {
	source := e.Source
	if source == "" {
		source = model.SourceWeb
	}

	row := model.AuditLog{
		At:           time.Now(),
		OperatorID:   optID(e.OperatorID),
		OperatorName: optStr(e.OperatorName),
		Source:       source,
		NodeID:       optID(e.NodeID),
		ResourceType: e.ResourceType,
		ResourceID:   optID(e.ResourceID),
		ResourceName: optStr(e.ResourceName),
		Action:       e.Action,
		Params:       toJSON(e.Params),
		BeforeState:  toJSON(e.BeforeState),
		AfterState:   toJSON(e.AfterState),
		Success:      e.Success,
		Error:        optStr(e.Error),
		ClientIP:     optStr(e.ClientIP),
	}

	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[audit] 写入失败 action=%s operator=%d resource=%s/%d err=%v",
			e.Action, e.OperatorID, e.ResourceType, e.ResourceID, err)
	}
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

// toJSON 把任意值序列化为 JSON 文本；零值时返回 nil（记为 NULL）。
//
// 序列化失败时返回占位说明而非报错——审计要能记下「记录本身出了问题」，
// 而不是因为记录失败而丢失整条事件。
func toJSON(v any) *string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return nil
		}
		return &s
	}
	b, err := json.Marshal(v)
	if err != nil {
		placeholder := `{"_error":"内容无法序列化"}`
		return &placeholder
	}
	s := string(b)
	return &s
}
