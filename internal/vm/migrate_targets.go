package vm

import (
	"context"
	"log"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// MigrateTargetView 是一个可作为迁移目标的节点（F-6-03 / F-6-04）。
//
// 它是"清单聚合"的落点：容量来自**最近一次周期采样**而不是按需探测——
// 打开迁移对话框时逐节点实时探测，节点一多就是一次探测风暴，而采样本来
// 就在按分钟级落库。
type MigrateTargetView struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`
	Online   bool   `json:"online"`
	// Maintenance 为 true 时不可作为目标（迁过去什么都做不了）。
	Maintenance bool `json:"maintenance"`

	// 容量快照。CPUCores / MemFreeMB 来自最近一条 host_stats_record；
	// StorageFreeGB 是全部就绪池的 Usable 汇总。StatsKnown 为 false 表示
	// 还没有采样（新接入的节点），容量栏显示"暂无采样"而不是 0。
	CPUCores      int   `json:"cpu_cores"`
	MemFreeMB     int64 `json:"mem_free_mb"`
	StorageFreeGB int64 `json:"storage_free_gb"`
	StatsKnown    bool  `json:"stats_known"`

	// Conflicts 是该节点上与这台虚拟机冲突的节点内唯一资源
	//（静态地址 / 端口转发），复用迁移受理时的同一套检查。
	Conflicts []string `json:"conflicts,omitempty"`

	// Suitable 为 false 时 Reason 说明原因。
	Suitable bool   `json:"suitable"`
	Reason   string `json:"reason,omitempty"`
	// Recommended 是"要是我来选"的建议：全部就绪目标里余量最大的那个。
	// 只标一个，且**只是建议**——放哪儿终究是人的决定。
	Recommended bool `json:"recommended"`
}

// MigrateTargets 返回一台虚拟机可选的迁移目标（F-6-04）。
//
// 与预检共用 ensureMigrateTarget / migrationConflicts 的判定，但**不探测
// 虚拟机状态**：这一步回答的是"有哪些地方可以去"，而"现在能不能走"是
// 预检（PreviewMigration）的事——两个问题分开问，界面上也就分两步展示。
func (s *Service) MigrateTargets(
	ctx context.Context, id int64, v authz.Viewer,
) ([]MigrateTargetView, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	var nodes []model.Node
	if err := s.db.WithContext(ctx).
		Where("id <> ? AND enroll_state = ?", target.NodeID, model.NodeEnrollEnrolled).
		Order("id").Find(&nodes).Error; err != nil {
		log.Printf("[vm] 查询迁移目标节点失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]MigrateTargetView, 0, len(nodes))
	best, bestScore := -1, int64(-1)
	for i := range nodes {
		n := &nodes[i]
		view := MigrateTargetView{
			NodeID: n.ID, NodeName: n.Name,
			Online: n.Status == model.NodeStatusOnline, Maintenance: n.MaintenanceMode,
		}

		// 容量：最近一条采样 + 就绪池的可用汇总。
		var stat model.HostStatsRecord
		err := s.db.WithContext(ctx).
			Where("node_id = ?", n.ID).Order("at DESC").First(&stat).Error
		if err == nil {
			view.StatsKnown = true
			view.CPUCores = stat.CPUCores
			view.MemFreeMB = stat.MemTotalMB - stat.MemUsedMB
			if view.MemFreeMB < 0 {
				view.MemFreeMB = 0
			}
		}
		var usable int64
		if err := s.db.WithContext(ctx).Model(&model.StoragePool{}).
			Where("node_id = ? AND status = ?", n.ID, model.StoragePoolReady).
			Select("COALESCE(SUM(usable_bytes), 0)").Scan(&usable).Error; err == nil {
			view.StorageFreeGB = usable / (1024 * 1024 * 1024)
		}

		// 冲突：与受理同一套检查，这里只是提前亮出来。
		conflicts, err := s.migrationConflicts(ctx, target, n.ID)
		if err != nil {
			return nil, err
		}
		view.Conflicts = conflicts

		switch {
		case n.MaintenanceMode:
			view.Reason = "节点处于维护模式，迁过去将无法开关机"
		case !view.Online:
			view.Reason = "节点离线"
		case len(conflicts) > 0:
			view.Reason = "节点内唯一资源冲突：" + joinConflicts(conflicts)
		default:
			view.Suitable = true
			// 建议分数：存储余量优先（磁盘是迁移要搬的主体），内存做次级
			// 参考。分数只在"有采样"的节点之间比较——拿没有数据的节点当
			// 建议等于在瞎猜。
			if view.StatsKnown {
				score := view.StorageFreeGB*1024 + view.MemFreeMB
				if score > bestScore {
					bestScore, best = score, len(out)
				}
			}
		}
		out = append(out, view)
	}
	if best >= 0 {
		out[best].Recommended = true
	}
	return out, nil
}
