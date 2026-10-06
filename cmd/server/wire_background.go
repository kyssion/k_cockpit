// wire_background 装配后台周期组件与调度器注册表（internal/service/ops/scheduler）。
//
// 周期组件天然跨域（配额循环在 ops 但要调 compute 的关机、介质弹出循环
// 属于 compute、TRIM 属于 storage），因此不按域拆，而按**生命周期**归组：
// 全部在这里构造、全部在 startBackground 启动、stopBackground 优雅收尾。
package main

import (
	"context"
	"time"

	"k_cockpit/internal/service/compute/vm"
	"k_cockpit/internal/service/ops/alert"
	"k_cockpit/internal/service/ops/monitor"
	"k_cockpit/internal/service/ops/quotaenforce"
	cron "k_cockpit/internal/service/ops/schedule"
	"k_cockpit/internal/service/ops/scheduler"
	sched "k_cockpit/internal/service/ops/scheduler"
	"k_cockpit/internal/service/ops/task"
	"k_cockpit/internal/service/platform/authkey"
	"k_cockpit/internal/service/platform/passaudit"
	"k_cockpit/internal/service/platform/settings"
	"k_cockpit/internal/service/storage/pool"
)

// backgroundLoops 承载全部后台周期组件（跨域，统一生命周期管理）。
type backgroundLoops struct {
	scheduler      *cron.Scheduler
	quotaLoop      *quotaenforce.Loop
	alertLoop      *alert.Loop
	schedRetention *scheduler.RetentionLoop
	trimLoop       *pool.TrimLoop
	mediaEjectLoop *vm.MediaEjectLoop
	passAuditLoop  *passaudit.Loop
	authKeyLoop    *authkey.Loop
	collector      *monitor.Collector
}

// setupSchedulerRegistry 登记调度器身份并建记录器。
//
// 必须先于所有周期组件（setupBackground）与 ops 的调度视图
// （setupOpsServices）调用：它们都要 Observe 这个记录器。
func (a *app) setupSchedulerRegistry() {
	// 调度器注册表（F-7-04）：先登记身份与说明，再把记录器交给各组件。
	//
	// **登记发生在启动之前**：界面上的「有哪些调度器在跑」来自这张表，
	// 而不是来自事件表。只靠事件的话，一个正常但最近无事可做的调度器
	// 会从列表里消失——而那与「它坏了」是两回事。
	a.schedRegistry = sched.NewRegistry()
	sched.RegisterBuiltins(a.schedRegistry, sched.BuiltinOptions{
		MetricsInterval:        monitor.DefaultOptions().Interval,
		MetricsCleanupInterval: monitor.DefaultOptions().CleanupInterval,
		ScheduleInterval:       cron.DefaultOptions().Interval,
		QueuePollInterval:      task.DefaultOptions().PollInterval,
		QuotaEvalInterval:      quotaenforce.DefaultOptions().Interval,
		PasswordAuditInterval:  24 * time.Hour,
		RetentionInterval:      time.Hour,
		TrimInterval:           24 * time.Hour,
		MediaEjectInterval:     time.Minute,
	})
	// 记录器在这里建好（在**所有**周期组件之前）：定时任务扫描器与采集器
	// 构造之后马上就要接上它。
	a.schedRecorder = sched.NewRecorder(a.db, a.schedRegistry)
}

// setupBackground 构造全部后台周期组件并接上调度记录器。
//
// 只构造与 Observe，不 Start——启动集中在 startBackground，以便与优雅退出
// （stopBackground）成对出现。观测必须在启动之前接上，否则启动后到接上之间
// 那一轮的动作不会被记录。
func (a *app) setupBackground() {
	// 定时任务扫描器。定时快照复用 vm 服务的创建快照入口：那条路要先建记录
	// 拿 ID、过配额、探测运行态。在调度器里重抄一遍等于把规则放两份。
	a.bg.scheduler = cron.New(a.db, a.queue, cron.Options{})
	a.bg.scheduler.SetSnapshotCreator(a.compute.vmSvc)
	a.bg.scheduler.Observe(a.schedRecorder)

	// 配额评估循环：配额以月计，5 分钟一轮足够，而它要扫两张按天累计的表。
	a.bg.quotaLoop = quotaenforce.NewLoop(a.ops.quotaEnforce, quotaenforce.DefaultOptions())
	// "关机"处置与用户通知（F-8-06）：关机复用 vm 的入队逻辑，通知只在
	// 已验证邮箱 + SMTP 可用时发出。
	a.ops.quotaEnforce.SetVMShutdown(a.compute.vmSvc.ShutdownUserVMsOnNode)
	a.ops.quotaEnforce.SetNotifier(a.notifyUserEmail)
	a.bg.quotaLoop.Observe(a.schedRecorder)

	// 告警评估循环（F-8-07）：只读库表，不探测节点——评估每五分钟跑一次，
	// 在里面探测会让告警系统自己成为负载。
	a.bg.alertLoop = alert.NewLoop(a.ops.alert, alert.DefaultOptions())
	a.bg.alertLoop.Observe(a.schedRecorder)

	// 口令安全检查循环（F-10-06）：一天一次，判定在节点侧完成。
	a.bg.passAuditLoop = passaudit.NewLoop(a.platform.passAudit, passaudit.DefaultOptions())
	// 命中通知（F-10-05）：发给命中者本人。
	a.platform.passAudit.SetNotifier(a.notifyUserEmail)
	a.bg.passAuditLoop.Observe(a.schedRecorder)

	// 会话密钥自动轮换（F-1-09）：间隔为 0 时这个循环什么都不做。
	a.bg.authKeyLoop = authkey.NewLoop(a.platform.authKey, func() int {
		return a.platform.settings.Int("security.auth_key_rotate_days", 0)
	}, authkey.DefaultOptions())
	a.bg.authKeyLoop.Observe(a.schedRecorder)

	// 指标采集器：按固定间隔落库。
	//
	// **它必须独立于页面访问**——「用户看页面时顺便采一次」得到的是密度由
	// 点击行为决定的伪历史：有人看的时候一秒一条，没人看的时候一条都没有。
	// 用它算出来的任何趋势都与真实情况无关，而它看起来像一份正常的图表。
	//
	// 采集有它自己的节奏，与谁在看无关。
	a.bg.collector = monitor.NewCollector(a.db, a.mockAgent, monitor.DefaultOptions())
	a.bg.collector.Observe(a.schedRecorder)

	// 调度事件保留清理（G-48）：保留期从设置读取，每次清理前现取值。
	a.bg.schedRetention = scheduler.NewRetentionLoop(a.db, a.schedRecorder, scheduler.RetentionOptions{
		KeepHours: func() int {
			return a.platform.settings.Int(settings.KeySchedulerEventKeepHours, 168)
		},
	})

	// 存储空间自动回收（G-52）：开关从设置读取，执行结果记入调度事件。
	a.bg.trimLoop = pool.NewTrimLoop(a.db, a.mockAgent, a.schedRecorder, pool.TrimOptions{
		Enabled: func() bool {
			return a.platform.settings.Bool(settings.KeyStorageAutoTrim, false)
		},
	})

	// 安装介质自动弹出（F-2-17）：Windows 初始化就绪后弹出安装 ISO。
	a.bg.mediaEjectLoop = vm.NewMediaEjectLoop(a.compute.vmSvc, a.db, a.mockAgent, a.schedRecorder, vm.MediaEjectOptions{})
}

// startBackground 启动全部后台周期组件。
func (a *app) startBackground() {
	ctx := context.Background()
	a.bg.scheduler.Start(ctx)
	a.bg.quotaLoop.Start(ctx)
	a.bg.alertLoop.Start(ctx)
	go a.bg.passAuditLoop.Start(ctx)
	go a.bg.authKeyLoop.Start(ctx)
	a.bg.collector.Start(ctx)
	go a.bg.schedRetention.Start(ctx)
	go a.bg.trimLoop.Start(ctx)
	go a.bg.mediaEjectLoop.Start(ctx)
}

// stopBackground 停止需要优雅收尾的周期组件。
//
// 只停这几个：与拆分前 main 里的 defer 一一对应。passAuditLoop /
// authKeyLoop / scheduler 原本就没有 Stop（进程退出即止），这里不新增。
func (a *app) stopBackground() {
	a.bg.collector.Stop()
	a.bg.alertLoop.Stop()
	a.bg.quotaLoop.Stop()
	a.bg.schedRetention.Stop()
	a.bg.trimLoop.Stop()
	a.bg.mediaEjectLoop.Stop()
}
