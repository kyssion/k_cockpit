// Package search 提供跨资源的快速检索（F-9-08）。
//
// 它存在的理由只有一句：**用户知道名字，但不知道去哪个页面找**。虚拟机、
// 节点、模板分散在各自的列表里，而"那台叫 web-03 的机器在哪个节点上"
// 这类问题，靠翻菜单回答不了。
//
// 这里不做全文检索：名字（与 IP）的前缀/包含匹配就够用，而且数据量在
// 面板的规模上（几百台）远没到需要索引的程度。为它引入一套检索引擎，
// 带来的运维负担远大于收益。
package search

import (
	"context"
	"log"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// 匹配类型。
const (
	KindVM       = "vm"
	KindNode     = "node"
	KindTemplate = "template"
)

// perKind 是每一类最多返回多少条。
//
// 搜索面板不是列表页：它的用途是"敲几个字然后跳过去"，超过 5 条同类结果
// 就要开始滚动，而那时用户更该去对应的列表页用筛选。
const perKind = 5

// Match 是一条命中。
type Match struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Subtitle 是副标题（节点名、状态等），用于区分同名资源。
	Subtitle string `json:"subtitle,omitempty"`
	// Link 是面板内路径，界面直接跳转。
	Link string `json:"link"`
}

// Service 提供检索。
type Service struct {
	db *gorm.DB
}

// NewService 构造服务。
func NewService(db *gorm.DB) *Service { return &Service{db: db} }

// Search 按关键字检索。
func (s *Service) Search(ctx context.Context, q string, v authz.Viewer) ([]Match, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []Match{}, nil
	}
	// 关键字过短时不做检索：单字符会命中几乎全部记录，而返回一堆无关结果
	// 比"没有结果"更浪费用户的时间。
	if len([]rune(q)) < 2 {
		return []Match{}, nil
	}
	like := "%" + q + "%"

	out := make([]Match, 0, perKind*3)
	out = append(out, s.searchVMs(ctx, like, v)...)
	out = append(out, s.searchTemplates(ctx, like, v)...)
	// 节点是平台级资源，只对管理员开放：租户不该通过搜索推断出"有哪些
	// 宿主机"——那是基础设施拓扑信息。
	if v.IsAdmin {
		out = append(out, s.searchNodes(ctx, like)...)
	}
	return out, nil
}

func (s *Service) searchVMs(ctx context.Context, like string, v authz.Viewer) []Match {
	query := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("present = ?", true).
		Where("name LIKE ? OR ip_summary LIKE ? OR COALESCE(remark, '') LIKE ?", like, like, like)
	if !v.IsAdmin {
		query = query.Where("owner_id = ?", v.UserID)
	}
	var rows []model.VM
	if err := query.Order("name").Limit(perKind).Find(&rows).Error; err != nil {
		log.Printf("[search] 检索虚拟机失败: %v", err)
		return nil
	}

	out := make([]Match, 0, len(rows))
	for i := range rows {
		vm := &rows[i]
		parts := []string{statusText(vm.Status)}
		if vm.IPSummary != nil && *vm.IPSummary != "" {
			parts = append(parts, *vm.IPSummary)
		}
		out = append(out, Match{
			Kind: KindVM, ID: vm.ID, Name: vm.Name,
			Subtitle: strings.Join(parts, " · "),
			Link:     "/vm/" + intToPath(vm.ID),
		})
	}
	return out
}

func (s *Service) searchNodes(ctx context.Context, like string) []Match {
	var rows []model.Node
	if err := s.db.WithContext(ctx).
		Where("name LIKE ? OR COALESCE(remark, '') LIKE ?", like, like).
		Order("name").Limit(perKind).Find(&rows).Error; err != nil {
		log.Printf("[search] 检索节点失败: %v", err)
		return nil
	}
	out := make([]Match, 0, len(rows))
	for i := range rows {
		out = append(out, Match{
			Kind:     KindNode,
			ID:       rows[i].ID,
			Name:     rows[i].Name,
			Subtitle: nodeSubtitle(&rows[i]),
			Link:     "/node/" + intToPath(rows[i].ID),
		})
	}
	return out
}

func (s *Service) searchTemplates(ctx context.Context, like string, v authz.Viewer) []Match {
	query := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("name LIKE ? OR COALESCE(remark, '') LIKE ?", like, like)
	if !v.IsAdmin {
		// 与模板列表同一套可见性规则：已发布，或自己创建的私有模板。
		query = query.Where("published = ? OR created_by = ?", true, v.UserID)
	}
	var rows []model.Template
	if err := query.Order("name").Limit(perKind).Find(&rows).Error; err != nil {
		log.Printf("[search] 检索模板失败: %v", err)
		return nil
	}
	out := make([]Match, 0, len(rows))
	for i := range rows {
		out = append(out, Match{
			Kind:     KindTemplate,
			ID:       rows[i].ID,
			Name:     rows[i].Name,
			Subtitle: templateSubtitle(&rows[i]),
			Link:     "/template",
		})
	}
	return out
}

func statusText(status string) string {
	switch status {
	case model.VMStatusRunning:
		return "运行中"
	case model.VMStatusStopped:
		return "已关机"
	case model.VMStatusPaused:
		return "已暂停"
	case model.VMStatusSuspended:
		return "已挂起"
	case model.VMStatusError:
		return "错误"
	}
	return "未知"
}

func nodeSubtitle(n *model.Node) string {
	if n.MaintenanceMode {
		return "维护中"
	}
	switch n.Status {
	case model.NodeStatusOnline:
		return "在线"
	case model.NodeStatusOffline:
		return "离线"
	}
	return "未知"
}

func templateSubtitle(t *model.Template) string {
	if !t.IsReady() {
		return "制备中"
	}
	if t.Published {
		return "已发布"
	}
	return "私有"
}

func intToPath(id int64) string { return strconv.FormatInt(id, 10) }
