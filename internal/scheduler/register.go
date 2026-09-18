package scheduler

import "time"

// RegisterBuiltins 登记项目里所有周期性调度器。
//
// 它们各自实现在不同的包里、跑法也不同（轮询、tick、批量），这里只登记
// **身份与说明**——注册表回答的是「有哪些东西在周期性跑」，而不是「它们
// 怎么跑」。
//
// Description 里必须写清「多久做一次」：界面上没有别的字段能告诉用户这一点，
// 而它是判断「这个调度器是不是还活着」的唯一依据。
func RegisterBuiltins(r *Registry, opts BuiltinOptions) {
	r.Register(Info{
		Key: KeyMetricsHost, Name: "宿主机指标采集", Group: GroupMetrics,
		Description:     "按节点逐个读取 CPU / 内存 / 网络 / 磁盘，写入指标明细。采不到时不写记录——写 0 会让图表显示「CPU 为 0%」，那看起来像机器很闲。",
		IntervalSeconds: int(opts.MetricsInterval.Seconds()),
	})
	r.Register(Info{
		Key: KeyMetricsGuest, Name: "虚拟机指标采集", Group: GroupMetrics,
		Description:     "只采运行中的虚拟机，并把运行时长与流量累加到当天。停机机器没有指标可读，给它们写记录会让图表上出现一条贴着 0 的线。",
		IntervalSeconds: int(opts.MetricsInterval.Seconds()),
	})
	r.Register(Info{
		Key: KeyMetricsDaily, Name: "指标明细清理", Group: GroupMaintain,
		Description:     "删除超过保留期的明细，按天聚合的表不动——一台机器一年才 365 行，而它才是配额与运行时长要回溯的东西。删了才记事件。",
		IntervalSeconds: int(opts.MetricsCleanupInterval.Seconds()),
	})
	r.Register(Info{
		Key: KeyScheduleScan, Name: "定时任务扫描", Group: GroupScheduled,
		Description:     "扫描到点的用户定时任务并入队。**每次触发单独记一条**（与采集器不同）——每条定时任务都对应一次用户可见的操作，用户需要能回答「我设的那条到底跑没跑」。",
		IntervalSeconds: int(opts.ScheduleInterval.Seconds()),
	})
	r.Register(Info{
		Key: KeyQuotaEvaluate, Name: "配额超限评估", Group: GroupQuota,
		Description:     "按当前周期重算各用户的流量与运行时长，跨越阈值时按策略处置（限速 / 断网）。**有变化才记事件**——绝大多数轮次里没有配额跨越阈值。",
		IntervalSeconds: int(opts.QuotaEvalInterval.Seconds()),
	})
	r.Register(Info{
		Key: KeyTaskQueue, Name: "任务队列派发", Group: GroupTasks,
		Description:     "把待执行任务派发给执行器，受并发上限与资源锁约束。**它不产生调度事件**——它执行的每个动作本身都已经是一条任务记录，在这里再记一遍只是把同一件事说两次。",
		IntervalSeconds: int(opts.QueuePollInterval.Seconds()),
	})
}

// BuiltinOptions 是登记时需要的各组件周期。
type BuiltinOptions struct {
	MetricsInterval        time.Duration
	MetricsCleanupInterval time.Duration
	ScheduleInterval       time.Duration
	QueuePollInterval      time.Duration
	QuotaEvalInterval      time.Duration
}
