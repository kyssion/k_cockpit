package dashboard

import (
	"context"
	"log"
	"sort"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/model"
	"k_cockpit/internal/quota"
	"k_cockpit/internal/quotaenforce"
)

// 本文件是工作台的「我的配额」区块（G-32）。
//
// 它是**聚合层**：计算配额、存储配额、累计型配额各自有服务与数据源，
// 这里只把它们按「用户 × 节点」拼到一起。刻意不在本文件重新实现任何
// 用量计算——聚合层自己算一份的结果，必然与判定层算出的那份漂移。

// UsedLimit 是一个维度的「已用 / 上限」。上限为 0 表示不限。
type UsedLimit struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
	// HasLimit 为 false 时界面显示「不限」而不是一个 0 上限的进度条。
	HasLimit bool `json:"has_limit"`
	// Unit 是展示单位（核 / MB / GB / 小时 / 台 / 个 / 条）。
	Unit string `json:"unit,omitempty"`
}

// NodeQuota 是一个节点上的全部配额维度。
type NodeQuota struct {
	NodeID   int64  `json:"node_id"`
	NodeName string `json:"node_name"`

	// 计算资源（存量型）：来自 computequota。
	VCPU     UsedLimit `json:"vcpu"`
	MemoryMB UsedLimit `json:"memory_mb"`
	VMs      UsedLimit `json:"vms"`

	// 存储配额（存量型）：来自 quota。
	StorageGB UsedLimit `json:"storage_gb"`

	// 累计型（本周期，UTC 月）：来自 resource_quota + 按天统计表。
	TrafficInGB  UsedLimit `json:"traffic_in_gb"`
	TrafficOutGB UsedLimit `json:"traffic_out_gb"`
	RuntimeHours UsedLimit `json:"runtime_hours"`

	// WorstStatus 是全部维度里最差的一个（ok / warned / limited）。
	// 界面用它决定节点卡片的颜色，而不是逐维度判断一遍。
	WorstStatus string `json:"worst_status"`
}

// QuotaOverview 是用户视角的配额总览。
type QuotaOverview struct {
	GeneratedAt string      `json:"generated_at"`
	Nodes       []NodeQuota `json:"nodes"`
}

// QuotaOverview 汇总当前用户在各节点上的配额与用量。
//
// 节点集合取三个来源的并集：名下虚拟机所在节点、有配额记录的节点、
// 有存储空间的节点。只按其中一个来源的话，用户会在「有配额但还没建机」
// 的节点上看不到任何东西，而那正是管理员刚给他开通、等他去用的场景。
func (s *Service) QuotaOverview(ctx context.Context, v authz.Viewer) (*QuotaOverview, error) {
	if v.IsAdmin {
		// 管理员没有「自己的配额」这个概念（他的额度通常没配）：界面走
		// 平台视图，这个接口对管理员没有意义。仍返回 200 与空列表，
		// 而不是 403——让前端不必为同一页面维护两条错误路径。
		return &QuotaOverview{GeneratedAt: nowRFC3339(), Nodes: []NodeQuota{}}, nil
	}
	if s.computeQuota == nil && s.storageQuota == nil && s.runtimeQuota == nil {
		// 没装配任何配额能力：返回空而不是报错，配额是可选能力。
		return &QuotaOverview{GeneratedAt: nowRFC3339(), Nodes: []NodeQuota{}}, nil
	}

	nodeIDs, err := s.quotaNodeIDs(ctx, v.UserID)
	if err != nil {
		return nil, err
	}

	out := &QuotaOverview{GeneratedAt: nowRFC3339(), Nodes: []NodeQuota{}}
	names := s.nodeNames(ctx, nodeIDs)

	for _, nodeID := range nodeIDs {
		nq := NodeQuota{NodeID: nodeID, NodeName: names[nodeID], WorstStatus: model.QuotaStatusOK}

		if s.computeQuota != nil {
			u, err := s.computeQuota.UsageFor(ctx, v.UserID, nodeID)
			if err != nil {
				return nil, err
			}
			nq.VCPU = dimOf(int64(u.VCPU), int64(u.QuotaVCPU), "核")
			nq.MemoryMB = dimOf(int64(u.MemoryMB), int64(u.QuotaMemMB), "MB")
			nq.VMs = dimOf(int64(u.VMCount), int64(u.QuotaVMs), "台")
			// 计算配额没有 warned/limited 状态机（Check 在超线时直接拒绝），
			// 因此它不参与 WorstStatus 的比较。
		}
		if s.storageQuota != nil {
			if usage, err := s.storageQuota.Usage(ctx, v.UserID, nodeID); err == nil {
				// 上限为 0 = 不限（quota 的约定），展示层据此显示「不限」。
				limit := int64(0)
				if !usage.Unlimited {
					limit = usage.QuotaBytes
				}
				nq.StorageGB = dimOf(usage.TotalBytes/bytesPerGB, limit/bytesPerGB, "GB")
			}
			// 读取失败不阻断整页：存储配额只是众多维度之一，一个节点读不到
			// 不该让用户看不到其余节点。
		}
		if s.runtimeQuota != nil {
			used, err := s.runtimeQuota.UsageFor(ctx, v.UserID, nodeID)
			if err != nil {
				return nil, err
			}
			limits := s.runtimeLimits(ctx, v.UserID, nodeID)
			nq.TrafficInGB = dimOf(used[model.QuotaDimTrafficIn], limits[model.QuotaDimTrafficIn], "GB")
			nq.TrafficOutGB = dimOf(used[model.QuotaDimTrafficOut], limits[model.QuotaDimTrafficOut], "GB")
			nq.RuntimeHours = dimOf(used[model.QuotaDimRuntime], limits[model.QuotaDimRuntime], "小时")
			nq.WorstStatus = worstOf(nq.WorstStatus,
				statusOf(nq.TrafficInGB), statusOf(nq.TrafficOutGB), statusOf(nq.RuntimeHours))
		}

		out.Nodes = append(out.Nodes, nq)
	}
	return out, nil
}

// quotaNodeIDs 收集「与用户有关」的节点：名下虚拟机、配额记录、存储空间。
func (s *Service) quotaNodeIDs(ctx context.Context, userID int64) ([]int64, error) {
	seen := map[int64]bool{}
	var ids []int64
	add := func(rows []int64, src string) error {
		for _, id := range rows {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		return nil
	}

	var vmNodes []int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("owner_id = ? AND present = ?", userID, true).
		Distinct("node_id").Pluck("node_id", &vmNodes).Error; err != nil {
		log.Printf("[dashboard] 查询用户节点失败: %v", err)
		return nil, api.Internal()
	}
	if err := add(vmNodes, "vm"); err != nil {
		return nil, err
	}

	var quotaNodes []int64
	if err := s.db.WithContext(ctx).Model(&model.ResourceQuota{}).
		Where("user_id = ?", userID).
		Distinct("node_id").Pluck("node_id", &quotaNodes).Error; err != nil {
		log.Printf("[dashboard] 查询配额节点失败: %v", err)
		return nil, api.Internal()
	}
	if err := add(quotaNodes, "resource_quota"); err != nil {
		return nil, err
	}

	var storageNodes []int64
	if err := s.db.WithContext(ctx).Model(&model.UserStorage{}).
		Where("user_id = ?", userID).
		Distinct("node_id").Pluck("node_id", &storageNodes).Error; err != nil {
		log.Printf("[dashboard] 查询存储节点失败: %v", err)
		return nil, api.Internal()
	}
	if err := add(storageNodes, "user_storage"); err != nil {
		return nil, err
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// runtimeLimits 读累计型配额的上限（没有记录的维度视为不限）。
func (s *Service) runtimeLimits(ctx context.Context, userID, nodeID int64) map[string]int64 {
	var rows []model.ResourceQuota
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ?", nodeID, userID).Find(&rows).Error; err != nil {
		log.Printf("[dashboard] 查询资源配额失败: %v", err)
		return map[string]int64{}
	}
	out := map[string]int64{}
	for i := range rows {
		out[rows[i].Dimension] = rows[i].LimitValue
	}
	return out
}

// nodeNames 批量取节点名；查不到的节点留空（已删除节点的历史配额行）。
func (s *Service) nodeNames(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var rows []model.Node
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err != nil {
		log.Printf("[dashboard] 查询节点名失败: %v", err)
		return out
	}
	for i := range rows {
		out[rows[i].ID] = rows[i].Name
	}
	return out
}

func dimOf(used, limit int64, unit string) UsedLimit {
	return UsedLimit{Used: used, Limit: limit, HasLimit: limit > 0, Unit: unit}
}

// statusOf 按已用比例给出维度状态（与 quotaenforce 的 80% 预警阈值同口径）。
func statusOf(d UsedLimit) string {
	if !d.HasLimit || d.Limit <= 0 {
		return model.QuotaStatusOK
	}
	switch {
	case d.Used >= d.Limit:
		return model.QuotaStatusLimited
	case d.Used*100 >= d.Limit*model.WarnRatioPercent:
		return model.QuotaStatusWarned
	}
	return model.QuotaStatusOK
}

// worstOf 取两者中更差的状态：ok < warned < limited。
func worstOf(a, b string, rest ...string) string {
	rank := map[string]int{model.QuotaStatusOK: 0, model.QuotaStatusWarned: 1, model.QuotaStatusLimited: 2}
	worst := a
	for _, s := range append([]string{b}, rest...) {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	return worst
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// bytesPerGB 是字节到 GB 的换算基数（与 quotaenforce 的口径一致）。
const bytesPerGB = 1 << 30

// 装配：三个配额服务都是**可选**依赖（nil 时对应维度不出现），
// 与 vm.Service 对配额校验器的可选注入是同一条约定。
func (s *Service) SetQuotaReaders(
	compute *computequota.Service,
	storage *quota.Service,
	runtime *quotaenforce.Service,
) {
	s.computeQuota = compute
	s.storageQuota = storage
	s.runtimeQuota = runtime
}
