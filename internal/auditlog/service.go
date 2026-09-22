// Package auditlog 提供审计流水的查询（F-1-12）。
//
// 在此之前，审计只有**写入**没有**读取**：audit.Recorder 一直在往
// audit_log 表里记，每一处高风险操作都记了，但没有任何人能查。数据在，
// 入口没有——这是"最亏"的一种缺口。
//
// 本包只做查询，不做写入（写入在 audit 包）。
//
// 有一条权限规则决定了这里的每一项设计：
//
//	**租户只能看自己的记录。**
//
// 审计条目里带着资源名、客户端 IP、操作参数与前后状态。把这些暴露给别的
// 租户，等于把"谁在什么时候动过什么"这张图交出去——而"对方有一台叫
// prod-db 的机器"这类信息本身就有价值。
package auditlog

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Service 提供审计查询。
type Service struct {
	db *gorm.DB
}

// NewService 构造服务。
func NewService(db *gorm.DB) *Service { return &Service{db: db} }

// Filter 是查询条件。
type Filter struct {
	// From / To 是时间范围；零值表示不限。
	From time.Time
	To   time.Time
	// OperatorID 按操作者筛选；**仅管理员可用**（租户永远被强制成自己）。
	OperatorID int64
	// ResourceType / Action 支持前缀匹配。
	//
	// 用前缀而不是精确：动作名是分层的（`vm.snapshot.create`），而用户
	// 想看的往往是"所有快照相关的动作"。要求他逐个列出十几个精确名称
	// 才能看全，等于让筛选形同虚设。
	ResourceType string
	Action       string
	ResourceID   int64
	NodeID       int64
	// Success 为 nil 表示不限。
	Success *bool
	// Keyword 在资源名与错误信息里做模糊匹配。
	Keyword string
	// Source 按记录来源精确匹配（web / api / system / emergency，G-38）；
	// 空串表示不限。
	Source string

	Page     int
	PageSize int
}

// EntryView 是一条审计记录的对外视图。
type EntryView struct {
	ID int64 `json:"id"`
	// At 是发生时刻。
	At string `json:"at"`

	OperatorID   *int64 `json:"operator_id,omitempty"`
	OperatorName string `json:"operator_name,omitempty"`
	// Source 区分这条记录来自界面还是 API 凭证。
	//
	// 它比看起来重要：一个用 API Key 执行的操作**不会触发二次验证**，
	// 因此"这条记录是怎么来的"是判断风险时的第一手信息。
	Source string `json:"source"`

	NodeID       *int64 `json:"node_id,omitempty"`
	ResourceType string `json:"resource_type"`
	ResourceID   *int64 `json:"resource_id,omitempty"`
	ResourceName string `json:"resource_name,omitempty"`
	Action       string `json:"action"`

	// Params / BeforeState / AfterState 是原始 JSON 文本。
	//
	// 返回字符串而不是解析后的对象：它们是**任意结构**，而把一份未知形状
	// 的 JSON 解析再序列化只会改变它——顺序、数字精度都可能不同，而审计
	// 记录的原始形态本身就是证据的一部分。
	Params      string `json:"params,omitempty"`
	BeforeState string `json:"before_state,omitempty"`
	AfterState  string `json:"after_state,omitempty"`

	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	ClientIP string `json:"client_ip,omitempty"`
}

// Page 是一页审计记录。
type Page struct {
	Items    []EntryView `json:"items"`
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	// Truncated 为 true 表示结果被截断（超过上限）。
	//
	// 需要它是因为**审计查询和普通列表的失败模式不同**：普通列表少几条
	// 用户不会在意，而审计少了几条会让"这段时间没发生过这件事"这个结论
	// 变成错的。因此宁可明确说"被截断了，请缩小范围"。
	Truncated bool `json:"truncated"`
	// Note 是给界面看的一句说明（如"结果被截断"）。
	Note string `json:"note,omitempty"`
}

// 一次查询最多返回多少条。
//
// 上限存在的理由是**这个查询没有天然的边界**：审计流水只增不减，而"全部"
// 会在一两年后变成几十万条。给一个上限并明确告诉调用方"被截断了"，比悄悄
// 返回前 N 条要好。
const maxPageSize = 200

// List 查询审计流水。
func (s *Service) List(ctx context.Context, f Filter, v authz.Viewer) (*Page, error) {
	query := s.db.WithContext(ctx).Model(&model.AuditLog{})

	// **权限隔离在这里做，而且只在这里做。**
	//
	// 把这条判断放在 handler 里、或者指望界面不传别家的 operator_id，都是
	// 把一条安全规则寄托在调用方的自觉上。放在这一个地方，之后无论谁加
	// 新的查询入口都绕不过它。
	if !v.IsAdmin {
		query = query.Where("operator_id = ?", v.UserID)
	} else if f.OperatorID > 0 {
		query = query.Where("operator_id = ?", f.OperatorID)
	}

	if !f.From.IsZero() {
		query = query.Where("at >= ?", f.From)
	}
	if !f.To.IsZero() {
		query = query.Where("at <= ?", f.To)
	}
	if f.ResourceType != "" {
		// 前缀匹配：动作与资源类型都是分层的，用户想看的往往是"这一类"。
		query = query.Where("resource_type LIKE ? ESCAPE '\\'", escapeLike(f.ResourceType)+"%")
	}
	if f.Action != "" {
		query = query.Where("action LIKE ? ESCAPE '\\'", escapeLike(f.Action)+"%")
	}
	if f.ResourceID > 0 {
		query = query.Where("resource_id = ?", f.ResourceID)
	}
	if f.NodeID > 0 {
		query = query.Where("node_id = ?", f.NodeID)
	}
	if f.Success != nil {
		query = query.Where("success = ?", *f.Success)
	}
	// 来源（G-38）：web / api / system / emergency。它是判断一条记录风险的
	// 第一手信息——API 凭证与带外脚本的操作需要能被单独拎出来看。
	if f.Source != "" {
		query = query.Where("source = ?", f.Source)
	}
	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		// 只在资源名与错误信息里找：把参数也纳入匹配，会让"搜一个常见的
		// 短词"返回一堆看起来无关的记录（因为参数里恰好出现了那个词）。
		like := "%" + escapeLike(kw) + "%"
		query = query.Where(
			"resource_name LIKE ? ESCAPE '\\' OR error LIKE ? ESCAPE '\\'", like, like)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		log.Printf("[auditlog] 统计失败: %v", err)
		return nil, api.Internal()
	}

	page, size := f.Page, f.PageSize
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = 50
	}
	truncated := false
	if size > maxPageSize {
		size = maxPageSize
		truncated = true
	}

	var rows []model.AuditLog
	if err := query.
		// 时间倒序 + id 倒序：同一毫秒内的多条记录（一次批量操作的产物）
		// 需要一个稳定的次序，否则翻页时可能重复或漏掉。
		Order("at DESC, id DESC").
		Offset((page - 1) * size).Limit(size).
		Find(&rows).Error; err != nil {
		log.Printf("[auditlog] 查询失败: %v", err)
		return nil, api.Internal()
	}

	items := make([]EntryView, 0, len(rows))
	for i := range rows {
		items = append(items, toView(&rows[i]))
	}

	out := &Page{
		Items: items, Total: total, Page: page, PageSize: size,
		Truncated: truncated,
	}
	if truncated {
		out.Note = "单页最多返回 200 条，结果已截断——请缩小时间范围或增加筛选条件"
	}
	return out, nil
}

// Get 按 id 读取一条记录。
func (s *Service) Get(ctx context.Context, id int64, v authz.Viewer) (*EntryView, error) {
	var row model.AuditLog
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if isNotFound(err) {
			return nil, api.NotFound("记录不存在")
		}
		log.Printf("[auditlog] 查询失败: %v", err)
		return nil, api.Internal()
	}
	// 非管理员看不到别人的记录，返回 **404 而不是 403**：403 会确认
	// "这个 id 存在"，让人能通过枚举推断出系统里有多少条记录、以及它们
	// 属于谁。
	if !v.IsAdmin && (row.OperatorID == nil || *row.OperatorID != v.UserID) {
		return nil, api.NotFound("记录不存在")
	}
	view := toView(&row)
	return &view, nil
}

// Facets 是一次查询可用的筛选项。
//
// 由服务端给出而不是让界面写死：动作名会随功能增加而变化，写死的列表迟早
// 与后端对不上，而"筛选里选不到某个刚加的动作"是一种很难被发现的缺陷。
type Facets struct {
	Actions       []string `json:"actions"`
	ResourceTypes []string `json:"resource_types"`
}

// Facets 返回该调用者**可见范围内**出现过的动作与资源类型。
func (s *Service) Facets(ctx context.Context, v authz.Viewer) (*Facets, error) {
	base := s.db.WithContext(ctx).Model(&model.AuditLog{})
	if !v.IsAdmin {
		base = base.Where("operator_id = ?", v.UserID)
	}

	var actions, types []string
	if err := base.Distinct("action").Order("action").Pluck("action", &actions).Error; err != nil {
		log.Printf("[auditlog] 查询动作列表失败: %v", err)
		return nil, api.Internal()
	}
	if err := base.Distinct("resource_type").Order("resource_type").
		Pluck("resource_type", &types).Error; err != nil {
		log.Printf("[auditlog] 查询资源类型失败: %v", err)
		return nil, api.Internal()
	}
	return &Facets{Actions: actions, ResourceTypes: types}, nil
}

// --- 内部 ---

func toView(row *model.AuditLog) EntryView {
	view := EntryView{
		ID: row.ID, At: row.At.Format("2006-01-02T15:04:05Z07:00"),
		Source: row.Source, ResourceType: row.ResourceType, Action: row.Action,
		Success:    row.Success,
		OperatorID: row.OperatorID, NodeID: row.NodeID, ResourceID: row.ResourceID,
	}
	view.OperatorName = deref(row.OperatorName)
	view.ResourceName = deref(row.ResourceName)
	view.Error = deref(row.Error)
	view.ClientIP = deref(row.ClientIP)

	// 三个 JSON 字段做一次**规范化**（压缩空白）而不是美化：它们是记录
	// 的原始形态，美化会在界面上引入换行把一条记录撑得很高，而规范化
	// 不改变语义。
	view.Params = compact(row.Params)
	view.BeforeState = compact(row.BeforeState)
	view.AfterState = compact(row.AfterState)
	return view
}

func compact(p *string) string {
	if p == nil || *p == "" {
		return ""
	}
	var buf any
	if err := json.Unmarshal([]byte(*p), &buf); err != nil {
		// 解析不了就原样返回：审计记录不该因为"格式不认识"而消失。
		return *p
	}
	out, err := json.Marshal(buf)
	if err != nil {
		return *p
	}
	return string(out)
}

// escapeLike 转义 LIKE 里的通配符。
//
// 不转义的话，用户搜 `%` 会命中一切——那看起来像"筛选没生效"，而实际是
// `%` 被当成了通配符。
//
// **光转义还不够，还必须带 `ESCAPE '\\'` 子句**，否则转义符自己会被当成
// 普通字符。这一点在两种数据库上表现不同：
//
//	PostgreSQL 的 LIKE **默认转义符就是反斜杠**，因此不带 ESCAPE 也能工作；
//	SQLite 没有默认转义符，不带 ESCAPE 时 `\%` 会被当成"反斜杠 + 任意字符"。
//
// 也就是说，只写转义的话，这个功能在生产上正确、在测试库上静默失效——
// 而测试库正是唯一会发现它坏掉的地方。带上 ESCAPE 之后两边行为一致。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	return strings.ReplaceAll(s, "_", "\\_")
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "record not found")
}
