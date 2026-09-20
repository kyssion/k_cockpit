package alert

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// want 是"此刻应当存在"的一条告警。
type want struct {
	NodeID       *int64
	Kind         string
	Level        string
	ResourceType string
	ResourceID   int64
	ResourceName string
	Title        string
	Detail       string
}

// Evaluate 评估一轮：把"此刻应当存在的告警"与库里对齐。
//
// 返回**发生变化**的条数。这个返回值是给调度事件用的：绝大多数轮次什么
// 都没变，那些轮次不该留下任何记录（与其它周期组件同一约定）。
func (s *Service) Evaluate(ctx context.Context) (int, error) {
	wanted, err := s.collect(ctx)
	if err != nil {
		return 0, err
	}

	// 去重：同一种类的同一个对象只保留一条。collect 里可能因为多处条件
	// 命中同一个对象（例如节点既离线又在维护中——那是两种不同的告警，
	// 但同一节点的同一种类不会重复）。
	seen := map[string]bool{}
	unique := make([]want, 0, len(wanted))
	for _, w := range wanted {
		key := w.Kind + "|" + w.ResourceType + "|" + strconv.FormatInt(w.ResourceID, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, w)
	}

	changed := 0
	now := time.Now()
	live := map[string]bool{}

	for _, w := range unique {
		live[w.Kind+"|"+w.ResourceType+"|"+strconv.FormatInt(w.ResourceID, 10)] = true

		var row model.Alert
		q := s.db.WithContext(ctx).Where(
			"kind = ? AND resource_type = ? AND resource_id = ?", w.Kind, w.ResourceType, w.ResourceID).
			First(&row)

		switch {
		case q.Error == nil:
			updates := map[string]any{
				"level":         w.Level,
				"title":         w.Title,
				"detail":        w.Detail,
				"last_at":       now,
				"node_id":       w.NodeID,
				"resource_name": w.ResourceName,
			}
			// 已经恢复过的告警重新出现：**重新变成未确认**，并把 first_at
			// 重置为这一次。沿用旧的 first_at 会让它看起来"已经响了三天"，
			// 而实际上中间好过——那是误导。
			if row.Status == model.AlertCleared {
				updates["status"] = model.AlertActive
				updates["first_at"] = now
				updates["ack_by"] = nil
				updates["ack_at"] = nil
				updates["cleared_at"] = nil
			}
			if err := s.db.WithContext(ctx).Model(&model.Alert{}).
				Where("id = ?", row.ID).Updates(updates).Error; err != nil {
				log.Printf("[alert] 更新告警失败 id=%d: %v", row.ID, err)
				continue
			}
			if row.Status == model.AlertCleared {
				changed++
			}
		case isNotFound(q.Error):
			row := model.Alert{
				NodeID: w.NodeID, Kind: w.Kind, Level: w.Level,
				ResourceType: w.ResourceType, ResourceID: w.ResourceID,
				ResourceName: w.ResourceName,
				Title:        w.Title, Detail: w.Detail,
				Status:  model.AlertActive,
				FirstAt: now, LastAt: now,
			}
			if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
				log.Printf("[alert] 写入告警失败 kind=%s: %v", w.Kind, err)
				continue
			}
			changed++
		default:
			log.Printf("[alert] 查询告警失败 kind=%s: %v", w.Kind, q.Error)
		}
	}

	// 这一轮没有再出现、但库里还开着的告警 → 关闭。
	//
	// **不删除**：一条告警恢复过这件事本身有价值（"上次是什么时候好的"），
	// 删掉之后无法判断这个问题是偶发还是持续。
	var open []model.Alert
	if err := s.db.WithContext(ctx).Where("status IN ?",
		[]string{model.AlertActive, model.AlertAcked}).Find(&open).Error; err != nil {
		log.Printf("[alert] 查询开启中的告警失败: %v", err)
		return changed, nil
	}
	for i := range open {
		a := &open[i]
		key := a.Kind + "|" + a.ResourceType + "|" + strconv.FormatInt(a.ResourceID, 10)
		if live[key] {
			continue
		}
		if err := s.db.WithContext(ctx).Model(&model.Alert{}).Where("id = ?", a.ID).
			Updates(map[string]any{
				"status":     model.AlertCleared,
				"cleared_at": now,
			}).Error; err != nil {
			log.Printf("[alert] 关闭告警失败 id=%d: %v", a.ID, err)
			continue
		}
		changed++
	}
	return changed, nil
}

// collect 收集"此刻应当存在"的告警。
//
// 只读数据库，不做任何探测：评估每五分钟跑一次，在里面探测节点会让告警
// 系统自己成为负载——而且节点不可达本来就会以"离线"的形式体现出来。
func (s *Service) collect(ctx context.Context) ([]want, error) {
	out := make([]want, 0, 16)

	// --- 节点 ---
	var nodes []model.Node
	if err := s.db.WithContext(ctx).Find(&nodes).Error; err != nil {
		log.Printf("[alert] 查询节点失败: %v", err)
		return nil, api.Internal()
	}
	for i := range nodes {
		n := &nodes[i]
		id := n.ID
		if !n.IsEnrolled() {
			out = append(out, want{
				NodeID: &id, Kind: KindNodePending, Level: model.AlertLevelWarning,
				ResourceType: "node", ResourceID: n.ID, ResourceName: n.Name,
				Title:  "节点 " + n.Name + " 尚未接入",
				Detail: "已生成注册令牌，但 agent 还没有完成注册。",
			})
		}
		if n.MaintenanceMode {
			out = append(out, want{
				NodeID: &id, Kind: KindNodeMaintenance, Level: model.AlertLevelWarning,
				ResourceType: "node", ResourceID: n.ID, ResourceName: n.Name,
				Title:  "节点 " + n.Name + " 处于维护模式",
				Detail: "维护期间该节点上的创建与电源操作会被拒绝，但仍可作为迁移目标承接迁出。",
			})
		}
		if n.Status == model.NodeStatusOffline {
			out = append(out, want{
				NodeID: &id, Kind: KindNodeOffline, Level: model.AlertLevelDanger,
				ResourceType: "node", ResourceID: n.ID, ResourceName: n.Name,
				Title:  "节点 " + n.Name + " 离线",
				Detail: "心跳超时，面板无法确认它上面的虚拟机状态。相关操作会被拒绝。",
			})
		}
	}

	// --- 失效的虚拟机（虚拟化层已不存在）---
	var missing []model.VM
	if err := s.db.WithContext(ctx).Where("present = ?", false).Find(&missing).Error; err != nil {
		log.Printf("[alert] 查询失效虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	for i := range missing {
		vm := &missing[i]
		id := vm.NodeID
		out = append(out, want{
			NodeID: &id, Kind: KindVMMissing, Level: model.AlertLevelWarning,
			ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
			Title:  "虚拟机 " + vm.Name + " 在虚拟化层已不存在",
			Detail: "它在面板之外被删除了。记录保留以便追溯，可在回收站里彻底删除。",
		})
	}

	// --- 近 24 小时失败的任务（按任务计，便于定位）---
	var failed []model.Task
	if err := s.db.WithContext(ctx).
		Where("status = ? AND finished_at >= ?", model.TaskFailed, time.Now().UTC().Add(-failedTaskWindow)).
		Find(&failed).Error; err != nil {
		log.Printf("[alert] 查询失败任务失败: %v", err)
		return nil, api.Internal()
	}
	for i := range failed {
		t := &failed[i]
		out = append(out, want{
			NodeID: t.NodeID, Kind: KindTaskFailed, Level: model.AlertLevelDanger,
			ResourceType: "task", ResourceID: t.ID, ResourceName: strValue(t.ResourceName),
			Title:  "任务 #" + itoa(int(t.ID)) + " 失败：" + strValue(t.ResourceName),
			Detail: "失败原因：" + strValue(t.Error),
		})
	}

	// --- 超限并被处置的配额 ---
	var quotas []model.ResourceQuota
	if err := s.db.WithContext(ctx).Where("status = ?", model.QuotaStatusLimited).
		Find(&quotas).Error; err != nil {
		log.Printf("[alert] 查询超限配额失败: %v", err)
		return nil, api.Internal()
	}
	for i := range quotas {
		q := &quotas[i]
		id := q.NodeID
		out = append(out, want{
			NodeID: &id, Kind: KindQuotaLimited, Level: model.AlertLevelWarning,
			ResourceType: "resource_quota", ResourceID: q.ID,
			Title:  "配额 " + model.QuotaDimLabel(q.Dimension) + " 已超限并被处置",
			Detail: strValue(q.Detail),
		})
	}

	// --- 存储与宿主机负载（按节点，取最近一次采样）---
	for i := range nodes {
		n := &nodes[i]
		id := n.ID

		var pool struct {
			Total  int64
			Usable int64
		}
		if err := s.db.WithContext(ctx).Model(&model.StoragePool{}).
			Select("COALESCE(SUM(total_bytes),0) AS total, COALESCE(SUM(usable_bytes),0) AS usable").
			Where("node_id = ?", n.ID).Scan(&pool).Error; err != nil {
			log.Printf("[alert] 汇总存储池失败 node=%d: %v", n.ID, err)
			continue
		}
		if pool.Total > 0 && pool.Usable < int64(storageLowGB)*1024*1024*1024 {
			out = append(out, want{
				NodeID: &id, Kind: KindStorageLow, Level: model.AlertLevelDanger,
				ResourceType: "node", ResourceID: n.ID, ResourceName: n.Name,
				Title:  "节点 " + n.Name + " 的存储剩余不足",
				Detail: "可用空间低于 " + itoa(storageLowGB) + " GB，新的虚拟机可能创建失败。",
			})
		}

		var rec model.HostStatsRecord
		if err := s.db.WithContext(ctx).Where("node_id = ?", n.ID).
			Order("at DESC").First(&rec).Error; err == nil {
			if rec.CPUPercent >= cpuHighPercent {
				out = append(out, want{
					NodeID: &id, Kind: KindHostCPU, Level: model.AlertLevelDanger,
					ResourceType: "node", ResourceID: n.ID, ResourceName: n.Name,
					Title:  "节点 " + n.Name + " 的 CPU 使用率偏高",
					Detail: "最近一次采样为 " + formatPercent(rec.CPUPercent) + "。",
				})
			}
			if rec.MemTotalMB > 0 &&
				float64(rec.MemUsedMB)/float64(rec.MemTotalMB)*100 >= memHighPercent {
				out = append(out, want{
					NodeID: &id, Kind: KindHostMemory, Level: model.AlertLevelDanger,
					ResourceType: "node", ResourceID: n.ID, ResourceName: n.Name,
					Title: "节点 " + n.Name + " 的内存使用率偏高",
					Detail: "最近一次采样为 " +
						formatPercent(float64(rec.MemUsedMB)/float64(rec.MemTotalMB)*100) + "。",
				})
			}
		}
	}

	return out, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

func strValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func formatPercent(v float64) string {
	if v >= 100 {
		return "100%"
	}
	return itoa(int(v)) + "%"
}

func itoa(n int) string { return strconv.Itoa(n) }
