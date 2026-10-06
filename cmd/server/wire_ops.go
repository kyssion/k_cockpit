// wire_ops 装配 internal/service/ops 域：监控、工作台、调度视图、告警、定时任务、
// 站点维护、诊断、配额处置、平台自检与全局搜索。
//
// 依赖最广的一个域：工作台要聚合 node / 调优状态，维护要复用 vm 的关机
// 入队——因此排在 platform / storage / network / compute 全部之后。
package main

import (
	"k_cockpit/internal/platform/version"
	"k_cockpit/internal/service/ops/alert"
	"k_cockpit/internal/service/ops/dashboard"
	"k_cockpit/internal/service/ops/diagnostics"
	"k_cockpit/internal/service/ops/hosttuning"
	"k_cockpit/internal/service/ops/maintenance"
	"k_cockpit/internal/service/ops/monitor"
	"k_cockpit/internal/service/ops/platformcheck"
	"k_cockpit/internal/service/ops/quotaenforce"
	cron "k_cockpit/internal/service/ops/schedule"
	sched "k_cockpit/internal/service/ops/scheduler"
	"k_cockpit/internal/service/ops/search"
	"k_cockpit/internal/service/platform/settings"
)

// opsServices 承载 internal/service/ops 域的服务实例（周期组件见 backgroundLoops）。
type opsServices struct {
	monitor    *monitor.Service
	hostTuning *hosttuning.Service
	dashboard  *dashboard.Service
	// schedulerSvc 是调度器注册表的查询视图（sched 为注册表包）。
	schedulerSvc *sched.Service

	alert        *alert.Service
	scheduleSvc  *cron.Service
	maintenance  *maintenance.Service
	diagnostics  *diagnostics.Service
	quotaEnforce *quotaenforce.Service
	platformChk  *platformcheck.Service
	search       *search.Service
}

// setupOpsServices 装配 ops 域全部服务。
func (a *app) setupOpsServices() {
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	a.ops.monitor = monitor.NewService(db)
	// 宿主机调优（KSM / zRAM / 嵌套虚拟化）。工作台要复用它读一次状态，
	// 因此先建出来而不是塞进 Deps 里现造——两个实例意味着两份缓存口径。
	a.ops.hostTuning = hosttuning.NewService(db, queue, mockAgent, a.recorder)

	// 工作台概览（F-8-03 / F-8-04）：只读聚合，节点运行态复用节点服务。
	a.ops.dashboard = dashboard.NewService(db, a.node.nodeSvc)
	// 工作台上的 KSM / zRAM 直接读调优服务（同一份口径），硬件与网络统计
	// 走 agent 的两个按需操作。
	a.ops.dashboard.SetTuning(a.ops.hostTuning)
	a.ops.dashboard.SetAgent(mockAgent)
	a.ops.schedulerSvc = sched.NewService(db, a.schedRegistry)

	// 告警中心（F-8-07）。
	a.ops.alert = alert.NewService(db)

	// 定时任务（F-7-05）：调度器到点把**已有的任务类型**入队，自己不做任何
	// 节点操作，因此不需要新的执行器——这也是它能在 mock 之上完整跑通的原因。
	a.ops.scheduleSvc = cron.NewService(db)

	// 站点维护（G-46）：逐节点接管节点维护模式，批量关机复用 vm 的入队逻辑。
	// 放在 vmSvc 之后装配——它要把 ShutdownAllOnNode 注入进去。
	a.ops.maintenance = maintenance.NewService(db, a.node.nodeSvc, a.recorder)
	a.ops.maintenance.SetVMShutdown(a.compute.vmSvc.ShutdownAllOnNode)

	a.ops.quotaEnforce = quotaenforce.NewService(db, queue, mockAgent, a.recorder)
	a.ops.diagnostics = diagnostics.NewService(db, a.platform.settings, a.schedRegistry, a.recorder)
	// 版本摘要进诊断包：排障时第一个要问的就是「跑的是哪个版本」，
	// 而它应当随包一起走，不必再让人回头去问。
	a.ops.diagnostics.Version = version.Summary()
	a.ops.platformChk = platformcheck.NewService(db, queue, mockAgent, a.recorder)
	a.ops.search = search.NewService(db)
}

// setupCrossWiring 完成**跨域接线**：两个域的服务互相引用，任何一方的
// setup 方法里做都会造成循环依赖，因此集中在这里（此时各域都已就绪）。
func (a *app) setupCrossWiring() {
	// 工作台「我的配额」（G-32）：三类配额的读数都从各自的判定服务取，
	// 保证用户看到的数字与判定时用的是同一份。
	a.ops.dashboard.SetQuotaReaders(
		a.compute.computeQuota, a.storage.quota, a.ops.quotaEnforce)
	// 非负载类自检提示（F-8-03）：每次取摘要时现读设置——改完配置刷新
	// 即消失，不等下一次评估周期。
	a.ops.dashboard.SetNotices(func() []dashboard.Alert {
		var out []dashboard.Alert
		if a.platform.settings.String("notification.smtp_host", "") == "" {
			out = append(out, dashboard.Alert{
				Level: "warning",
				Text:  "未配置邮件发信（SMTP）：找回密码与邀请注册的邮件发不出去",
				Link:  "/settings",
			})
		}
		if a.platform.settings.String(settings.KeySiteURL, "") == "" {
			out = append(out, dashboard.Alert{
				Level: "warning",
				Text:  "未配置站点对外地址：邀请链接是站外打不开的相对路径",
				Link:  "/settings",
			})
		}
		return out
	})
	// 公网地址与端口转发的数量同样受计算配额约束：它们都是稀缺资源，
	// 而"先到先得"通常不是管理员想要的分配策略。
	a.network.publicIP.SetComputeQuota(a.compute.computeQuota)
}
