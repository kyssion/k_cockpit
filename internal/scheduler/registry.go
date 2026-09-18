// Package scheduler 是周期性调度器的注册表与事件记录（F-7-04）。
//
// 它与 internal/schedule 是**两件不同的事**，名字相近但职责不相交：
//
//	schedule   = 用户定义的定时任务（F-7-05）：「每天早上 6 点关掉这台机器」
//	scheduler  = 系统内置的周期性工作（F-7-04）：「每 30 秒扫一次到点的定时任务」
//
// 换句话说，schedule 里的调度器本身也是在这里被观察的对象之一。
//
// 这个包不实现调度引擎——项目里已经有三个各自跑着的周期组件（任务队列轮询、
// 定时任务扫描、指标采集）。它们的实现方式不同（轮询、tick、批量），强行
// 抽成一套统一引擎的收益很小、风险很大。
//
// 它做的是另一件事：**让这些已经在跑的工作变得可查询**，并给它们一个统一的
// 地方记录「我实际做了什么」。
package scheduler

import (
	"sort"
	"sync"
)

// 分组名。分组是为了让界面上的列表可读——十几个调度器平铺在一起时，
// 用户看不出哪些属于同一套机制。
const (
	GroupMetrics   = "指标采集"
	GroupTasks     = "任务队列"
	GroupScheduled = "定时任务"
	GroupMaintain  = "数据维护"
	GroupQuota     = "配额与计量"
)

// 内置调度器的标识。
//
// 写成常量而不是散在各包里的字符串：这些 key 会进事件表，拼错一个字母不会
// 报错，只会让那一类事件永远与注册表对不上——而那种错误从界面上看不出来。
const (
	KeyTaskQueue     = "task.queue.poll"
	KeyTaskCleanup   = "task.queue.cleanup"
	KeyScheduleScan  = "schedule.scan"
	KeyMetricsHost   = "metrics.host"
	KeyMetricsGuest  = "metrics.guest"
	KeyMetricsDaily  = "metrics.daily"
	KeyRiskCleanup   = "risk.grant.cleanup"
	KeyQuotaEvaluate = "quota.evaluate"
)

// Info 描述一个已注册的调度器。
type Info struct {
	// Key 是稳定标识。
	Key string `json:"key"`
	// Name 是给人看的名字。
	Name string `json:"name"`
	// Group 是分组。
	Group string `json:"group"`
	// Description 说明它**做什么**、以及多久做一次。
	//
	// 要写清「多久做一次」：界面上没有任何别的字段能告诉用户这一点，
	// 而它是判断「这个调度器是不是还活着」的唯一依据。
	Description string `json:"description"`
	// Interval 是周期。
	IntervalSeconds int `json:"interval_seconds"`
	// RecordsOnlyOnAction 为 true 表示它**只在有动作时记录事件**。
	//
	// 恒为 true（见 model.SchedulerEvent 的说明），但仍然带出来：界面上
	// 需要据此显示「没有事件属于正常」，否则空的事件列表会被读成故障。
	RecordsOnlyOnAction bool `json:"records_only_on_action"`
}

// Registry 保存已注册的调度器。
type Registry struct {
	mu    sync.RWMutex
	items map[string]Info
}

// NewRegistry 构造注册表。
func NewRegistry() *Registry {
	return &Registry{items: make(map[string]Info)}
}

// Register 登记一个调度器。
func (r *Registry) Register(info Info) {
	r.mu.Lock()
	defer r.mu.Unlock()
	info.RecordsOnlyOnAction = true
	r.items[info.Key] = info
}

// Get 返回某个调度器的信息。
func (r *Registry) Get(key string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.items[key]
	return info, ok
}

// All 返回全部调度器，按**分组 + 名字**排序。
//
// 排序在这里做而不是交给界面：同一分组内的顺序应当稳定，否则每次刷新
// 列表都可能换一个次序，而一个会跳动的列表很难被扫读。
func (r *Registry) All() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Info, 0, len(r.items))
	for _, info := range r.items {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Grouped 按分组返回调度器，分组顺序按组名稳定排列。
func (r *Registry) Grouped() []Group {
	all := r.All()
	index := map[string]int{}
	out := []Group{}
	for _, info := range all {
		i, ok := index[info.Group]
		if !ok {
			i = len(out)
			index[info.Group] = i
			out = append(out, Group{Name: info.Group})
		}
		out[i].Schedulers = append(out[i].Schedulers, info)
	}
	return out
}

// Group 是一个分组及其下的调度器。
type Group struct {
	Name       string `json:"name"`
	Schedulers []Info `json:"schedulers"`
}
