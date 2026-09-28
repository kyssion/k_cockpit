// Package maintenance 实现站点级维护模式（G-46）。
//
// 语义与单机面板**刻意不同**：对方是"关掉全部虚拟机 + 停宿主机服务"，
// 因为面板就跑在那台机器上；我们是多节点控制面，照抄会在演示环境里表现
// 为「全部变灰但一台都没真关」。因此这里定义为：
//
//   - 进入：按节点逐个进入维护（复用节点维护模式），可选先为运行中的
//     虚拟机逐台入队优雅关机，逐节点汇总结果；
//   - 阻止开机：不需要新判断——VM 的创建与电源操作本来就查节点维护状态
//     （vm.ensureNodeUsable），节点全部维护中即全站禁止开机；
//   - 退出：只解除**本次由站点模式接管**的节点，管理员手工设置的维护
//     不受牵连。
package maintenance

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
)

// Service 提供站点维护模式。
type Service struct {
	db    *gorm.DB
	nodes *node.Service
	audit *audit.Recorder
	// shutdownNode 为节点上的运行中虚拟机入队关机任务（由 vm 服务提供，
	// 经 main 装配；不接时只进维护不关机）。做成函数而非直接依赖：
	// 注入窄函数即可，不必让本包背上 vm 包的完整依赖面。
	shutdownNode func(ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string) (int, error)
}

// NewService 构造服务。
func NewService(db *gorm.DB, nodes *node.Service, recorder *audit.Recorder) *Service {
	return &Service{db: db, nodes: nodes, audit: recorder}
}

// SetVMShutdown 装配批量关机（由 vm.Service.ShutdownAllOnNode 提供）。
func (s *Service) SetVMShutdown(fn func(ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string) (int, error)) {
	s.shutdownNode = fn
}

// EnterRequest 是进入维护的请求。
type EnterRequest struct {
	// Reason 必填：维护原因会写进每个节点的维护记录，事后要能回答
	// "为什么那天全部进维护了"。
	Reason string `json:"reason"`
	// ShutdownVMs 为 true 时，先为每个节点上运行中的虚拟机入队优雅关机。
	ShutdownVMs bool `json:"shutdown_vms"`
}

// NodeResult 是单个节点的处理结果。
type NodeResult struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	// Status 取值 maintained（本次接管）/ skipped_manual（本来就在维护中）/
	// skipped_not_enrolled（未接入，无维护可言）。
	Status string `json:"status"`
	// VMShutdowns 是本次为该节点入队的关机任务数。
	VMShutdowns int `json:"vm_shutdowns"`
	// Error 非空表示该节点处理失败（其余节点不受影响）。
	Error string `json:"error,omitempty"`
}

// View 是站点维护的当前状态。
type View struct {
	InMaintenance bool   `json:"in_maintenance"`
	Reason        string `json:"reason,omitempty"`
	ShutdownVMs   bool   `json:"shutdown_vms"`
	EnteredAt     string `json:"entered_at,omitempty"`
	EnteredByName string `json:"entered_by_name,omitempty"`
	// Nodes 只在 Enter 的响应里返回（逐节点汇总）。
	Nodes []NodeResult `json:"nodes,omitempty"`
	// ManagedNodes 是当前被站点模式接管的节点名（Status 查询用）。
	ManagedNodes []string `json:"managed_nodes,omitempty"`
}

// Enter 进入站点维护：逐节点接管并汇总结果。
func (s *Service) Enter(
	ctx context.Context, req EnterRequest, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return nil, api.InvalidParameter("请填写维护原因：它会写进每个节点的维护记录")
	}

	row, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if row.InMaintenance {
		return nil, api.Conflict("站点已处于维护模式")
	}

	var nodes []model.Node
	if err := s.db.WithContext(ctx).Order("id").Find(&nodes).Error; err != nil {
		log.Printf("[maintenance] 查询节点失败: %v", err)
		return nil, api.Internal()
	}

	// 逐节点处理，单节点失败不阻断其余：站点维护的目标是"尽快让全部
	// 节点安静下来"，一个节点的失败不该把其余节点留在可写状态。
	// 真正的维护状态是节点上的标志位，即使下面写 site 行失败，已接管
	// 的节点也不会再接受电源操作——最坏情况是"部分维护但状态行没记上"，
	// 这比"全部没维护"更接近用户要的结果。
	var (
		results []NodeResult
		managed []int64
	)
	for i := range nodes {
		n := &nodes[i]
		result := NodeResult{NodeID: n.ID, NodeName: n.Name}

		if !n.IsEnrolled() {
			result.Status = "skipped_not_enrolled"
			results = append(results, result)
			continue
		}
		if n.MaintenanceMode {
			// 管理员手工设置的维护**不接管**：退出站点维护时要能保留它。
			result.Status = "skipped_manual"
			results = append(results, result)
			continue
		}

		if req.ShutdownVMs && s.shutdownNode != nil {
			count, err := s.shutdownNode(ctx, n.ID, v, operatorName, clientIP)
			result.VMShutdowns = count
			if err != nil {
				result.Error = "关机任务入队失败：" + err.Error()
			}
		}
		// 关机任务入队失败也照样进维护：维护拦的是新的受理，不影响
		// 已在队列中的关机任务执行。
		if _, err := s.nodes.SetMaintenance(ctx, n.ID, true, reason, v.UserID, operatorName, clientIP); err != nil {
			result.Error = firstErr(result.Error, "进入维护失败："+err.Error())
			results = append(results, result)
			continue
		}
		result.Status = "maintained"
		managed = append(managed, n.ID)
		results = append(results, result)
	}

	now := time.Now()
	updates := map[string]any{
		"in_maintenance":  true,
		"reason":          reason,
		"shutdown_vms":    req.ShutdownVMs,
		"entered_by":      v.UserID,
		"entered_by_name": operatorName,
		"entered_at":      now,
		"node_ids":        joinIDs(managed),
	}
	if err := s.db.WithContext(ctx).Model(&model.SiteMaintenance{}).
		Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		// 状态行写失败时仍然返回汇总：节点的维护已生效，用户需要知道
		// 实际接管了哪些，而不是拿到一个 500 后对着界面猜。
		log.Printf("[maintenance] 写入站点维护状态失败: %v", err)
	}

	s.record(ctx, v, operatorName, clientIP, "maintenance.site.enter", map[string]any{
		"reason":       reason,
		"shutdown_vms": req.ShutdownVMs,
		"nodes":        len(managed),
	})

	return &View{InMaintenance: true, Reason: reason, ShutdownVMs: req.ShutdownVMs,
		EnteredAt: now.Format(time.RFC3339), EnteredByName: operatorName, Nodes: results}, nil
}

// Exit 退出站点维护：只解除本次接管的节点。
func (s *Service) Exit(ctx context.Context, v authz.Viewer, operatorName, clientIP string) (*View, error) {
	row, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	if !row.InMaintenance {
		return nil, api.Conflict("站点不在维护模式")
	}

	ids := splitIDs(row.NodeIDs)
	var failed []string
	for _, id := range ids {
		if _, err := s.nodes.SetMaintenance(ctx, id, false, "", v.UserID, operatorName, clientIP); err != nil {
			failed = append(failed, strconv.FormatInt(id, 10)+"("+err.Error()+")")
		}
	}

	// 有节点解不开时**不退出站点状态**：状态行还记着它，管理员可以再点
	// 一次退出（幂等），而不是让一个"半退出"被界面当成"已退出"。
	if len(failed) > 0 {
		return nil, api.Conflict("部分节点退出维护失败，站点维护状态保持不变，请重试：" +
			strings.Join(failed, "、"))
	}

	if err := s.db.WithContext(ctx).Model(&model.SiteMaintenance{}).
		Where("id = ?", row.ID).Updates(map[string]any{
		"in_maintenance":  false,
		"reason":          nil,
		"shutdown_vms":    false,
		"entered_by":      nil,
		"entered_by_name": "",
		"entered_at":      nil,
		"node_ids":        "",
	}).Error; err != nil {
		log.Printf("[maintenance] 清除站点维护状态失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, v, operatorName, clientIP, "maintenance.site.exit", map[string]any{
		"nodes": len(ids),
	})
	return &View{InMaintenance: false}, nil
}

// Status 返回当前状态与被接管的节点名。
func (s *Service) Status(ctx context.Context) (*View, error) {
	row, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	view := &View{InMaintenance: row.InMaintenance, ShutdownVMs: row.ShutdownVMs}
	if row.Reason != nil {
		view.Reason = *row.Reason
	}
	if !row.InMaintenance {
		return view, nil
	}
	if row.EnteredAt != nil {
		view.EnteredAt = row.EnteredAt.Format(time.RFC3339)
	}
	view.EnteredByName = row.EnteredByName

	ids := splitIDs(row.NodeIDs)
	if len(ids) == 0 {
		return view, nil
	}
	var nodes []model.Node
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&nodes).Error; err != nil {
		return view, nil // 名字取不到只影响展示，不影响状态本身。
	}
	for i := range nodes {
		view.ManagedNodes = append(view.ManagedNodes, nodes[i].Name)
	}
	return view, nil
}

// load 读取唯一的状态行；不存在视为「从未进入过维护」。
func (s *Service) load(ctx context.Context) (*model.SiteMaintenance, error) {
	var row model.SiteMaintenance
	err := s.db.WithContext(ctx).Where("id = ?", 1).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 迁移铺的行可能不存在（旧库未执行 0048）：动态补一行，比让
		// Status 一直 500 到有人去跑迁移更符合"查询不该失败"的预期。
		row = model.SiteMaintenance{ID: 1}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			log.Printf("[maintenance] 初始化状态行失败: %v", err)
			return nil, api.Internal()
		}
		return &row, nil
	}
	if err != nil {
		log.Printf("[maintenance] 查询状态失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) record(ctx context.Context, v authz.Viewer, operatorName, clientIP, action string, params map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		ResourceType: "site",
		ResourceName: "maintenance",
		Action:       action,
		Params:       params,
		Success:      true,
		ClientIP:     clientIP,
	})
}

func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func splitIDs(s string) []int64 {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func firstErr(a, b string) string {
	if a != "" {
		return a + "；" + b
	}
	return b
}
