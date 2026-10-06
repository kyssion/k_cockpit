// routes_ops 登记运行与观测域的路由：任务中心与 SSE、调度器视图、监控、
// 工作台、告警、站点维护、宿主机调优、平台自检、资源配额处置、诊断导出
// 与跨资源检索。
package router

import (
	"github.com/cloudwego/hertz/pkg/route"

	"k_cockpit/internal/handler/ops"
)

func registerOpsRoutes(v1 *route.RouterGroup, deps Deps, g guards) {
	taskHandler := ops.NewTask(deps.Task, deps.Bus)
	schedulerHandler := ops.NewScheduler(deps.Scheduler)
	monitorHandler := ops.NewMonitor(deps.Monitor)
	dashboardHandler := ops.NewDashboard(deps.Dashboard)
	diagnosticsHandler := ops.NewDiagnostics(deps.Diagnostics)
	quotaEnforceHandler := ops.NewQuotaEnforce(deps.QuotaEnforce)
	tuningHandler := ops.NewHostTuning(deps.HostTuning)
	platformCheckHandler := ops.NewPlatformCheck(deps.PlatformCheck)
	searchHandler := ops.NewSearch(deps.Search)
	alertHandler := ops.NewAlert(deps.Alert)
	maintenanceHandler := ops.NewMaintenance(deps.Maintenance, deps.Risk)

	{
		// 站点级维护（G-46）：逐节点接管 + 汇总。进入需要二次验证（handler
		// 内调用 guard），退出只解除本次接管的节点、无破坏性，不再多一道验证。
		v1.GET("/maintenance", g.requireAuth, g.adminOnly, maintenanceHandler.Status)
		v1.POST("/maintenance/enter", g.requireAuth, g.adminOnly, maintenanceHandler.Enter)
		v1.POST("/maintenance/exit", g.requireAuth, g.adminOnly, maintenanceHandler.Exit)

		// 指标历史（F-8-01 / F-8-02）。
		//
		// 数据由**独立采集器按固定间隔写入**，不是「用户看页面时顺便采一次」
		// ——后者的密度由点击行为决定，用它算「过去一周的负载」会得到与真实
		// 情况毫无关系的结果，而它看起来像一份正常的图表。
		//
		// 默认时间范围是最近 1 小时：不给默认值的话，一次不带参数的调用会
		// 扫全表，而数据攒了几个月之后那会慢到让人以为接口挂了。
		v1.GET("/monitor/host", g.requireAuth, g.adminOnly, monitorHandler.HostSeries)
		// 可筛选的物理设备（网卡 / 磁盘）。清单来自最近一次采样而不是现探一次：
		// 设备名几乎不变，而为打开一个下拉框去节点上取一次不值得。
		v1.GET("/monitor/host/devices", g.requireAuth, g.adminOnly, monitorHandler.HostDevices)
		v1.GET("/vms/:id/monitor", g.requireAuth, monitorHandler.VMSeries)
		v1.GET("/vms/:id/runtime", g.requireAuth, monitorHandler.Runtime)

		// 工作台概览（F-8-03 / F-8-04）。
		//
		// **不要求管理员**：租户也有自己的工作台。范围由服务层按视角收敛，
		// 而不是靠路由层切断——后者的结果是租户打开首页就 403，而首页是
		// 登录后第一个到达的页面。
		v1.GET("/dashboard/summary", g.requireAuth, dashboardHandler.Summary)
		// G-32：普通用户的配额总览（计算 / 存储 / 累计型逐节点聚合）。
		v1.GET("/dashboard/quotas", g.requireAuth, dashboardHandler.MyQuotas)
		// 宿主机细节（调优 / 硬件 / 网络统计）。它需要**按节点**向节点发请求，
		// 因此不并进 Summary——否则首页会变成一次探测风暴。
		v1.GET("/dashboard/host-detail", g.requireAuth, dashboardHandler.HostDetail)

		// 告警中心（F-8-07）：与工作台的横幅同源，但这里要能翻、能确认。
		v1.GET("/alerts", g.requireAuth, alertHandler.List)
		v1.POST("/alerts/:id/ack", g.requireAuth, alertHandler.Ack)
		v1.POST("/alerts/ack-all", g.requireAuth, alertHandler.AckAll)

		// 资源配额与超限处置（F-4-10）。
		//
		// 归管理员：配额是**跨用户的资源分配**，一个租户给自己调额度等于
		// 没有配额。
		//
		// 「提高上限会清除处置状态」写进了接口说明：不清的话，用户明明已经
		// 合规，网络却还是慢的，而界面上显示「已超限」。
		v1.GET("/resource-quotas", g.requireAuth, g.adminOnly, quotaEnforceHandler.List)
		v1.PUT("/resource-quotas", g.requireAuth, g.adminOnly, quotaEnforceHandler.Set)
		v1.DELETE("/resource-quotas/:id", g.requireAuth, g.adminOnly, quotaEnforceHandler.Delete)
		// 重置用量（F-1-09）：清的是**本周期的累计**，而不只是把状态改回正常。
		// 只清状态的话，下一次评估（五分钟内）会立刻重新判定为超限。
		v1.POST("/resource-quotas/:id/reset-usage", g.requireAuth, g.adminOnly, quotaEnforceHandler.ResetUsage)

		// 宿主机性能调优（KSM / ZRAM / 嵌套虚拟化 / CPU 亲和）。
		//
		// 归管理员：这几项改的是**宿主机自己**的行为——ZRAM 会占掉一块
		// 物理内存、嵌套虚拟化会向所有来宾暴露虚拟化扩展。租户不该有能力
		// 影响同宿主上别人的机器。
		v1.GET("/host/tuning", g.requireAuth, g.adminOnly, tuningHandler.Get)
		v1.PUT("/host/tuning", g.requireAuth, g.adminOnly, tuningHandler.Apply)
		v1.GET("/host/cpu-affinity-presets", g.requireAuth, g.adminOnly, tuningHandler.ListPresets)
		v1.POST("/host/cpu-affinity-presets", g.requireAuth, g.adminOnly, tuningHandler.CreatePreset)
		v1.DELETE("/host/cpu-affinity-presets/:id", g.requireAuth, g.adminOnly, tuningHandler.DeletePreset)

		// 平台自检与修复（F-4-13）。
		//
		// 自检与探测的区别：探测回答「有没有装」，自检回答「我们配的东西
		// 现在还在不在」——**面板显示「已启用」而节点上早就没了**，那种
		// 状态不会以任何形式报警，自检正是去找它。
		//
		// client-ip 不设 adminOnly：它返回的是**请求者自己的地址**，与任何
		// 外部「查本机 IP」服务等价；端口转发的来源白名单（用户功能）需要
		// 它做快速填充，限成管理员等于让普通用户手抄地址。
		v1.GET("/network/client-ip", g.requireAuth, platformCheckHandler.ClientIP)
		v1.GET("/ovs/status", g.requireAuth, g.adminOnly, platformCheckHandler.OVSStatus)
		v1.GET("/ovs/ports", g.requireAuth, g.adminOnly, platformCheckHandler.OVSPorts)
		v1.GET("/ovs/leases", g.requireAuth, g.adminOnly, platformCheckHandler.Leases)
		v1.POST("/ovs/check", g.requireAuth, g.adminOnly, platformCheckHandler.Check)
		v1.POST("/ovs/repair", g.requireAuth, g.adminOnly, platformCheckHandler.Repair)

		// 调度器框架（F-7-04）。
		//
		// 归管理员：这里暴露的是**系统内部的运行细节**（有哪些周期任务、
		// 最近做了什么、哪些失败了）。它对运维排查有用，对租户没有意义。
		//
		// 两个接口都只读——调度器是代码里注册的，不能从界面上开关。
		// 一个能被随手关掉的指标采集器，会让「指标为什么断了」变成一个
		// 需要翻操作日志才能回答的问题。
		v1.GET("/schedulers", g.requireAuth, g.adminOnly, schedulerHandler.Overview)
		v1.GET("/scheduler-events", g.requireAuth, g.adminOnly, schedulerHandler.Events)

		// 诊断导出（F-9-03）。
		//
		// 归管理员：包里是整个系统的内部状态（全部设置、全部审计日志），
		// 而审计日志里含别人的操作记录。
		//
		// **不需要二次验证**，这是刻意的：它最常见的用法是"出问题的时候
		// 赶紧导出来发给支持"，而在那一刻再拦一道验证会让最需要它的时候
		// 最不好用。敢这样的前提是**脱敏发生在打包时**——包里不含密钥
		// 明文，本身就是可以外发的。
		v1.GET("/diagnostics/categories", g.requireAuth, g.adminOnly, diagnosticsHandler.Categories)
		v1.GET("/diagnostics/export", g.requireAuth, g.adminOnly, diagnosticsHandler.Export)

		// 跨资源检索（F-9-08）：按名字找虚拟机 / 节点 / 模板。
		//
		// **不限制角色**，但服务层按视角收敛结果——搜索框看着只是"帮你找
		// 东西"，实际能枚举出整个平台的资源名，因此它是最容易意外泄漏信息
		// 的入口之一。
		v1.GET("/search", g.requireAuth, searchHandler.Query)

		// 任务中心：tenant 只能看到自己发起的（同样由归属过滤保证）。
		v1.GET("/tasks", g.requireAuth, taskHandler.List)
		// 任务状态流（SSE）。它替代任务中心与底部任务栏的轮询。
		v1.GET("/tasks/stream", g.requireAuth, taskHandler.Stream)
		v1.GET("/tasks/:id", g.requireAuth, taskHandler.Get)
		v1.POST("/tasks/:id/cancel", g.requireAuth, taskHandler.Cancel)
		// 清理已完成的旧任务。**只清终态**，且**被引用的不删**——
		// vm_schedule.last_task_id 与 network_capture.task_id 是当前状态
		// 的一部分，清掉它们指向的任务之后那些引用会悬空。
		v1.POST("/tasks/clear", g.requireAuth, g.adminOnly, taskHandler.ClearTasks)
	}
}
