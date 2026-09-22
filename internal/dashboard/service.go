// Package dashboard 实现工作台概览（F-8-03 管理员 / F-8-04 租户）。
//
// 两条贯穿本包的约束：
//
//   - **只读，且只读缓存**：所有数字要么来自控制面的投影（vm / node /
//     storage_pool 表），要么来自采集器按固定间隔落库的采样
//     （host_stats_record）。它**不在请求路径上直连虚拟化层**——首页是打开
//     最频繁的页面，让它去逐节点探测等于把探测频率交给用户的刷新行为。
//   - **数字缺了就说缺了**：没有采样时 `host` 为 nil，没有节点时不编造节点
//     卡片。一个 0 与"没采到"在界面上是两种截然不同的信息，而 0 看起来
//     完全正常。
//
// 为什么是一个聚合接口而不是让前端拼六七个请求：拼装的代价不止是请求数。
// 「宿主机资源」要是按节点去拉实时指标，那是 N+1 次探测——节点一多，打开
// 一次首页就变成一次对全部节点的探测风暴。聚合留在服务端一次做完。
package dashboard

import (
	"context"
	"log"
	"strconv"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
	"k_cockpit/internal/quota"
	"k_cockpit/internal/quotaenforce"
)

// 视角范围。
const (
	// ScopePlatform 是管理员看到的全景。
	ScopePlatform = "platform"
	// ScopeSelf 是租户看到的「我自己的」。
	ScopeSelf = "self"
)

// 最近虚拟机的行数。
//
// 5 行而不是 10 行：它的用途是「回来时一眼看到最近这几台」，再往下翻是
// 虚拟机列表页的职责。首页越长，真正要看的东西就越往下沉。
const recentLimit = 5

// Service 提供工作台概览的聚合读取。
type Service struct {
	db *gorm.DB
	// node 用于取节点运行态。
	//
	// 复用节点服务而不是自己再推一次心跳：离线的判定规则（阈值 40s、
	// 未接入即未知）只有一份是对的，抄一份到这里的那天起，两块界面就会
	// 对同一台节点给出不同的状态。
	node *node.Service
	// tuning 提供 KSM / zRAM 状态；agent 用于读取硬件与网络统计。
	//
	// 两者都可为 nil：工作台主体（计数、配额、告警）不依赖它们，缺了
	// 只是少了两块展示，不该让整个首页失败。
	tuning TuningProvider
	agent  agent.Client

	// 三个配额读数服务（G-32）。都是**可选**依赖：nil 时「我的配额」里
	// 对应的维度不出现，而不是报错——配额是可选能力，与 vm.Service 的
	// 可选注入是同一条约定。
	computeQuota *computequota.Service
	storageQuota *quota.Service
	runtimeQuota *quotaenforce.Service
}

// SetAgent 装配 agent 客户端；不调用时硬件与网络统计两区块不显示。
func (s *Service) SetAgent(c agent.Client) { s.agent = c }

// NewService 构造服务。
func NewService(db *gorm.DB, nodes *node.Service) *Service {
	return &Service{db: db, node: nodes}
}

// Summary 是工作台的一次快照。
type Summary struct {
	GeneratedAt string `json:"generated_at"`
	Scope       string `json:"scope"`

	// Nodes 仅管理员有值；租户看节点既无意义也越权。
	Nodes *NodeSummary `json:"nodes,omitempty"`

	VMs        VMSummary   `json:"vms"`
	Allocation Allocation  `json:"allocation"`
	Host       *HostUsage  `json:"host,omitempty"`
	Tasks      TaskSummary `json:"tasks"`
	Alerts     []Alert     `json:"alerts"`
	RecentVMs  []RecentVM  `json:"recent_vms"`
}

// NodeSummary 是节点的计数。
type NodeSummary struct {
	Total       int `json:"total"`
	Online      int `json:"online"`
	Offline     int `json:"offline"`
	Maintenance int `json:"maintenance"`
	// Pending 是已生成令牌但 agent 尚未注册的节点。
	Pending int `json:"pending"`
}

// VMSummary 是虚拟机的计数。
type VMSummary struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Stopped int `json:"stopped"`
	// Other 是暂停 / 挂起 / 错误 / 未知。
	//
	// 不逐项返回：首页上它们共享一个「非运行非关机」的含义，拆开只会让
	// 卡片变成一张状态字典。
	Other int `json:"other"`
	// Locked 是被业务软锁的台数（F-2-12）。
	Locked int `json:"locked"`
	// Missing 是在虚拟化层已不存在的台数。
	Missing int `json:"missing"`
}

// Allocation 是**已承诺给虚拟机**的资源。
//
// 它与 Host 的分工需要说清楚：Host 是宿主机**实际**用了多少（采样值），
// 这里是不管用没用、已经划出去多少。两者不是同一个量纲，把它们混成一条
// 进度条会得到「看起来 120%」这种既无法解释也无法行动的数字。
type Allocation struct {
	VCPU     int `json:"vcpu"`
	MemoryMB int `json:"memory_mb"`
	DiskGB   int `json:"disk_gb"`
	// RunningVCPU / RunningMemoryMB 只统计运行中的机器。
	//
	// 与上面的总量分开：已关机机器占着磁盘（配额口径要算它），但并不占用
	// 宿主机的 CPU 与内存。混在一起会让「还能不能开一台」这个问题得到
	// 一个偏保守到不可用的答案。
	RunningVCPU     int `json:"running_vcpu"`
	RunningMemoryMB int `json:"running_memory_mb"`
}

// HostUsage 是宿主机的**实际**用量，来自最近一次采样。
type HostUsage struct {
	// SampledNodes 是有采样的节点数。它与节点总数不是一回事：刚接入的
	// 节点还没轮到采集，界面上要能区分「没有数据」与「这台机器很闲」。
	SampledNodes   int     `json:"sampled_nodes"`
	CPUPercent     float64 `json:"cpu_percent"`
	CPUCores       int     `json:"cpu_cores"`
	MemUsedMB      int64   `json:"mem_used_mb"`
	MemTotalMB     int64   `json:"mem_total_mb"`
	DiskTotalBytes int64   `json:"disk_total_bytes"`
	DiskUsedBytes  int64   `json:"disk_used_bytes"`
	SampledAt      string  `json:"sampled_at"`
}

// TaskSummary 是任务的计数。
type TaskSummary struct {
	// Active 是尚未落定的任务（含 unknown）。
	//
	// 把 unknown 算进来是刻意的：它表示「执行中但结果未知」，此时任务
	// 仍然占着资源锁，用户仍然在等。漏掉它会让「明明有任务在跑」与
	// 「界面显示 0 个进行中」同时成立。
	Active    int `json:"active"`
	Failed24h int `json:"failed_24h"`
}

// Alert 是一条需要用户去看一眼的事。
type Alert struct {
	// Level 取值 danger / warning。
	Level string `json:"level"`
	Text  string `json:"text"`
	// Link 是面板内的路径，供界面直接跳过去。
	Link string `json:"link"`
}

// RecentVM 是「最近虚拟机」里的一行。
type RecentVM struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	NodeName  string `json:"node_name"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	CreatedAt string `json:"created_at"`
}

// Summary 返回工作台概览。
func (s *Service) Summary(ctx context.Context, v authz.Viewer) (*Summary, error) {
	out := &Summary{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Scope:       ScopeSelf,
		Alerts:      []Alert{},
		RecentVMs:   []RecentVM{},
	}
	if v.IsAdmin {
		out.Scope = ScopePlatform
	}

	nodes, err := s.nodes(ctx, v)
	if err != nil {
		return nil, err
	}
	out.Nodes = nodes

	vms, err := s.vms(ctx, v)
	if err != nil {
		return nil, err
	}
	out.VMs = vms

	alloc, err := s.allocation(ctx, v)
	if err != nil {
		return nil, err
	}
	out.Allocation = alloc

	if v.IsAdmin {
		host, err := s.host(ctx)
		if err != nil {
			return nil, err
		}
		out.Host = host
	}

	tasks, err := s.tasks(ctx, v)
	if err != nil {
		return nil, err
	}
	out.Tasks = tasks

	limited, err := s.limitedQuotas(ctx, v)
	if err != nil {
		return nil, err
	}

	recent, err := s.recentVMs(ctx, v)
	if err != nil {
		return nil, err
	}
	out.RecentVMs = recent

	out.Alerts = buildAlerts(nodes, vms, tasks, limited)
	return out, nil
}

// vmScope 给查询加上「存在」与归属过滤。
//
// present = false 的机器是「虚拟化层已经没有了」，它们不该出现在任何计数
// 里——但会以 Missing 的形式单独报出来，因为那本身就是一条要处理的事。
func vmScope(q *gorm.DB, v authz.Viewer) *gorm.DB {
	q = q.Where("present = ?", true)
	if !v.IsAdmin {
		q = q.Where("owner_id = ?", v.UserID)
	}
	return q
}

func (s *Service) nodes(ctx context.Context, v authz.Viewer) (*NodeSummary, error) {
	if !v.IsAdmin {
		return nil, nil
	}
	views, err := s.node.List(ctx)
	if err != nil {
		// 节点列表自己已经记过日志，这里不重复。
		return nil, err
	}
	out := &NodeSummary{Total: len(views)}
	for i := range views {
		n := &views[i]
		if n.EnrollState != model.NodeEnrollEnrolled {
			out.Pending++
		}
		if n.MaintenanceMode {
			out.Maintenance++
		}
		switch n.Status {
		case model.NodeStatusOnline:
			out.Online++
		case model.NodeStatusOffline:
			out.Offline++
		}
	}
	return out, nil
}

func (s *Service) vms(ctx context.Context, v authz.Viewer) (VMSummary, error) {
	var out VMSummary
	var rows []struct {
		Status string
		N      int64
	}
	err := vmScope(s.db.WithContext(ctx).Model(&model.VM{}), v).
		Select("status, count(*) AS n").
		Group("status").Scan(&rows).Error
	if err != nil {
		log.Printf("[dashboard] 统计虚拟机状态失败: %v", err)
		return out, api.Internal()
	}
	for _, r := range rows {
		out.Total += int(r.N)
		switch r.Status {
		case model.VMStatusRunning:
			out.Running = int(r.N)
		case model.VMStatusStopped:
			out.Stopped = int(r.N)
		default:
			out.Other += int(r.N)
		}
	}

	// 锁定数要连到虚拟机上才能做归属过滤，因此走子查询而不是直接 count。
	lock := s.db.WithContext(ctx).Model(&model.VMLock{}).Where("locked = ?", true)
	if !v.IsAdmin {
		lock = lock.Where("vm_id IN (?)",
			s.db.Model(&model.VM{}).Select("id").Where("owner_id = ?", v.UserID))
	}
	var locked int64
	if err := lock.Count(&locked).Error; err != nil {
		log.Printf("[dashboard] 统计锁定虚拟机失败: %v", err)
		return out, api.Internal()
	}
	out.Locked = int(locked)

	missing := s.db.WithContext(ctx).Model(&model.VM{}).Where("present = ?", false)
	if !v.IsAdmin {
		missing = missing.Where("owner_id = ?", v.UserID)
	}
	var gone int64
	if err := missing.Count(&gone).Error; err != nil {
		log.Printf("[dashboard] 统计失效虚拟机失败: %v", err)
		return out, api.Internal()
	}
	out.Missing = int(gone)
	return out, nil
}

func (s *Service) allocation(ctx context.Context, v authz.Viewer) (Allocation, error) {
	var row struct {
		VCPU            int64 `gorm:"column:vcpu"`
		MemoryMB        int64 `gorm:"column:memory_mb"`
		DiskGB          int64 `gorm:"column:disk_gb"`
		RunningVCPU     int64 `gorm:"column:running_vcpu"`
		RunningMemoryMB int64 `gorm:"column:running_memory_mb"`
	}
	err := vmScope(s.db.WithContext(ctx).Model(&model.VM{}), v).
		Select(`COALESCE(SUM(vcpu), 0) AS vcpu,
			COALESCE(SUM(memory_mb), 0) AS memory_mb,
			COALESCE(SUM(disk_gb), 0) AS disk_gb,
			COALESCE(SUM(CASE WHEN status = ? THEN vcpu ELSE 0 END), 0) AS running_vcpu,
			COALESCE(SUM(CASE WHEN status = ? THEN memory_mb ELSE 0 END), 0) AS running_memory_mb`,
			model.VMStatusRunning, model.VMStatusRunning).
		Scan(&row).Error
	if err != nil {
		log.Printf("[dashboard] 汇总已分配资源失败: %v", err)
		return Allocation{}, api.Internal()
	}
	return Allocation{
		VCPU:            int(row.VCPU),
		MemoryMB:        int(row.MemoryMB),
		DiskGB:          int(row.DiskGB),
		RunningVCPU:     int(row.RunningVCPU),
		RunningMemoryMB: int(row.RunningMemoryMB),
	}, nil
}

// host 汇总宿主机的实际用量。
//
// 取**每个节点最新一条**采样，而不是「最近 N 分钟的全部」：后者在采样间隔
// 内会重复计同一个节点，内存一栏立刻翻倍。
func (s *Service) host(ctx context.Context) (*HostUsage, error) {
	var rows []model.HostStatsRecord
	err := s.db.WithContext(ctx).
		Where("id IN (?)",
			s.db.Model(&model.HostStatsRecord{}).Select("MAX(id)").Group("node_id")).
		Find(&rows).Error
	if err != nil {
		log.Printf("[dashboard] 查询宿主机采样失败: %v", err)
		return nil, api.Internal()
	}
	if len(rows) == 0 {
		// 没有采样就返回 nil：给出一堆 0 会让人以为宿主机闲得反常。
		return nil, nil
	}

	out := &HostUsage{SampledNodes: len(rows)}
	var cpuSum float64
	var latest time.Time
	for i := range rows {
		r := &rows[i]
		out.CPUCores += r.CPUCores
		out.MemUsedMB += r.MemUsedMB
		out.MemTotalMB += r.MemTotalMB
		cpuSum += r.CPUPercent
		if r.At.After(latest) {
			latest = r.At
		}
	}
	out.CPUPercent = cpuSum / float64(len(rows))
	out.SampledAt = latest.UTC().Format(time.RFC3339)

	// 存储口径取**存储池**而不是整块宿主机磁盘：面板能分配的只有纳进池的
	// 那部分，用整盘做分母会让「还能建多少」这个问题得到过于乐观的答案。
	var pool struct {
		Total  int64 `gorm:"column:total"`
		Usable int64 `gorm:"column:usable"`
	}
	err = s.db.WithContext(ctx).Model(&model.StoragePool{}).
		Select("COALESCE(SUM(total_bytes), 0) AS total, COALESCE(SUM(usable_bytes), 0) AS usable").
		Scan(&pool).Error
	if err != nil {
		log.Printf("[dashboard] 汇总存储池容量失败: %v", err)
		return nil, api.Internal()
	}
	out.DiskTotalBytes = pool.Total
	out.DiskUsedBytes = pool.Total - pool.Usable
	return out, nil
}

func (s *Service) tasks(ctx context.Context, v authz.Viewer) (TaskSummary, error) {
	var out TaskSummary
	scope := s.db.WithContext(ctx).Model(&model.Task{})
	if !v.IsAdmin {
		scope = scope.Where("owner_id = ?", v.UserID)
	}
	var active int64
	err := scope.Where("status IN ?", []string{
		model.TaskPending, model.TaskRunning, model.TaskUnknown,
	}).Count(&active).Error
	if err != nil {
		log.Printf("[dashboard] 统计进行中任务失败: %v", err)
		return out, api.Internal()
	}
	out.Active = int(active)

	since := time.Now().UTC().Add(-24 * time.Hour)
	scope = s.db.WithContext(ctx).Model(&model.Task{}).
		Where("status = ? AND finished_at >= ?", model.TaskFailed, since)
	if !v.IsAdmin {
		scope = scope.Where("owner_id = ?", v.UserID)
	}
	var failed int64
	if err := scope.Count(&failed).Error; err != nil {
		log.Printf("[dashboard] 统计失败任务失败: %v", err)
		return out, api.Internal()
	}
	out.Failed24h = int(failed)
	return out, nil
}

// limitedQuotas 返回因超限已被处置的配额条数。
func (s *Service) limitedQuotas(ctx context.Context, v authz.Viewer) (int, error) {
	q := s.db.WithContext(ctx).Model(&model.ResourceQuota{}).
		Where("status = ?", model.QuotaStatusLimited)
	if !v.IsAdmin {
		q = q.Where("user_id = ?", v.UserID)
	}
	var n int64
	if err := q.Count(&n).Error; err != nil {
		log.Printf("[dashboard] 统计超限配额失败: %v", err)
		return 0, api.Internal()
	}
	return int(n), nil
}

func (s *Service) recentVMs(ctx context.Context, v authz.Viewer) ([]RecentVM, error) {
	var vms []model.VM
	err := vmScope(s.db.WithContext(ctx).Model(&model.VM{}), v).
		Order("created_at DESC").Limit(recentLimit).Find(&vms).Error
	if err != nil {
		log.Printf("[dashboard] 查询最近虚拟机失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]RecentVM, 0, len(vms))
	if len(vms) == 0 {
		return out, nil
	}

	// 节点名一次性取回：`node_id IN (...)` 而不是逐台查——后者是首页上
	// 最容易写出来的 N+1（五台机器五次查询，而为的只是五个名字）。
	ids := make([]int64, 0, len(vms))
	seen := make(map[int64]bool, len(vms))
	for i := range vms {
		if !seen[vms[i].NodeID] {
			seen[vms[i].NodeID] = true
			ids = append(ids, vms[i].NodeID)
		}
	}
	var nodes []model.Node
	if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&nodes).Error; err != nil {
		log.Printf("[dashboard] 查询节点名失败: %v", err)
		return nil, api.Internal()
	}
	names := make(map[int64]string, len(nodes))
	for i := range nodes {
		names[nodes[i].ID] = nodes[i].Name
	}

	for i := range vms {
		vm := &vms[i]
		out = append(out, RecentVM{
			ID:        vm.ID,
			Name:      vm.Name,
			Status:    vm.Status,
			NodeName:  names[vm.NodeID],
			VCPU:      vm.VCPU,
			MemoryMB:  vm.MemoryMB,
			CreatedAt: vm.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// buildAlerts 把计数翻译成「需要去看一眼」的事。
//
// 阈值即存在性：任何一条告警都不设「低于百分之几就不报」的门槛。工作台
// 是唯一一个用户每都会经过的页面，而"有一台机器在虚拟化层已经消失了"
// 这种事，无论多少都该让他看见——静默的代价由用户承担，省下的只是几行字。
func buildAlerts(nodes *NodeSummary, vms VMSummary, tasks TaskSummary, limited int) []Alert {
	out := make([]Alert, 0, 4)
	if nodes != nil {
		if nodes.Offline > 0 {
			out = append(out, Alert{
				Level: "danger", Link: "/node",
				Text: plural(nodes.Offline, "个节点离线"),
			})
		}
		if nodes.Pending > 0 {
			out = append(out, Alert{
				Level: "warning", Link: "/node",
				Text: plural(nodes.Pending, "个节点等待接入"),
			})
		}
		if nodes.Maintenance > 0 {
			out = append(out, Alert{
				Level: "warning", Link: "/node",
				Text: plural(nodes.Maintenance, "个节点处于维护模式"),
			})
		}
		if nodes.Total == 0 {
			out = append(out, Alert{
				Level: "warning", Link: "/node",
				Text: "尚未接入任何节点",
			})
		}
	}
	if vms.Missing > 0 {
		out = append(out, Alert{
			Level: "warning", Link: "/vm",
			Text: plural(vms.Missing, "台虚拟机在虚拟化层已不存在"),
		})
	}
	if tasks.Failed24h > 0 {
		out = append(out, Alert{
			Level: "danger", Link: "/task",
			Text: "近 24 小时有 " + strconv.Itoa(tasks.Failed24h) + " 个任务失败",
		})
	}
	if limited > 0 {
		out = append(out, Alert{
			Level: "warning", Link: "/resource-quota",
			Text: plural(limited, "项配额已超限并被处置"),
		})
	}
	return out
}

// plural 生成「N 个……」。调用方都在计数大于 0 时才进来，因此这里不为
// 零值单独写一条路径——一条不会走到的分支比没有更费解。
func plural(n int, unit string) string {
	return strconv.Itoa(n) + " " + unit
}
