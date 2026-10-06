// routes_compute 登记计算域的路由：虚拟机全生命周期（创建向导、电源、
// 快照、磁盘、光驱、网络管理、控制台、迁移、导出、重装、救援、回收站、
// 强删兜底、存量纳管）、模板与派生链、镜像导入、标签、定时任务、
// 计算配额与 PCIe 直通。
//
// 定时任务（scheduleHandler）虽然是 ops 包的 handler，但路由全部挂在
// /vms/:id 之下——按资源归属放在这里构造与登记。
package router

import (
	"github.com/cloudwego/hertz/pkg/route"

	"k_cockpit/internal/handler/compute"
	"k_cockpit/internal/handler/ops"
)

func registerComputeRoutes(v1 *route.RouterGroup, deps Deps, g guards) {
	vmHandler := compute.NewVM(deps.VM, deps.Risk)
	consoleHandler := compute.NewConsole(deps.VM, deps.Risk)
	templateHandler := compute.NewTemplate(deps.Template)
	importerHandler := compute.NewImporter(deps.Importer)
	tagHandler := compute.NewVMTag(deps.VMTag)
	passthroughHandler := compute.NewPassthrough(deps.Passthrough)
	computeQuotaHandler := compute.NewComputeQuota(deps.ComputeQuota)
	scheduleHandler := ops.NewSchedule(deps.Schedule, deps.Risk)

	{
		// 存量虚拟机纳管（节点接入的后续动作）：扫描面板之外的域并把
		// 选中的登记进来。仅管理员；纳管只补记录、不动域，无需二次验证。
		v1.GET("/nodes/:id/unmanaged-vms", g.requireAuth, g.adminOnly, vmHandler.UnmanagedDomains)
		v1.POST("/nodes/:id/adopt-vm", g.requireAuth, g.adminOnly, vmHandler.AdoptDomain)

		// 虚拟机：管理员可操作全部，tenant 仅自己名下（归属过滤在数据访问层注入，
		// 因此这里不需要按角色分路由）。
		v1.GET("/vms", g.requireAuth, vmHandler.List)
		v1.GET("/vms/:id", g.requireAuth, vmHandler.Get)
		// 创建表单元数据（F-2-02）：字段、取值、默认值与前置条件**由后端
		// 下发**，前端不维护第二份规则。放在 /vms/:id 之前注册，避免被
		// 通配路径吃掉。
		v1.GET("/vms/create-form", g.requireAuth, vmHandler.CreateForm)
		v1.POST("/vms", g.requireAuth, vmHandler.Create)
		// 批量操作（F-2-01）：电源与删除。逐台独立受理、可部分成功，
		// 因此始终返回 200，逐台的结果在响应体里。
		//
		// 删除动作在此处走一次二次验证（本文件是角色与验证要求的集中声明处）。
		v1.POST("/vms/batch-actions", g.requireAuth, vmHandler.BatchAction)

		// 业务软锁（F-2-12）。同步生效——锁只在控制面，虚拟化层不知道它。
		// 解锁需要二次验证，因此下面这条路由也受 risk 保护（在 handler 内声明）。
		v1.PATCH("/vms/:id/lock", g.requireAuth, vmHandler.SetLock)

		// 模板管理与模板克隆（F-3-01 / F-3-02）。
		//
		// 读接口对所有登录用户开放——可见性（已发布 / 自己创建的）由服务层
		// 过滤，不在路由层按角色一刀切：私有模板的所有者本来就应该能看到
		// 自己的东西，哪怕他只是 tenant。
		v1.GET("/templates", g.requireAuth, templateHandler.List)
		v1.GET("/templates/:id", g.requireAuth, templateHandler.Get)
		v1.POST("/templates", g.requireAuth, templateHandler.CreateFromVM)
		v1.PATCH("/templates/:id", g.requireAuth, templateHandler.Update)
		// 删除前的检查。**只读**，且与 Delete 共用同一段判定——两处各写一遍
		// 迟早分叉，而分叉的表现是「预览说可以删、点下去却报冲突」。
		v1.GET("/templates/:id/delete-preview", g.requireAuth, templateHandler.DeletePreview)
		// 模板族：同一条派生链上的全部版本（F-3-04）。放在 :id 通配之后也不会
		// 被吃掉，因为它多一段路径。
		v1.GET("/templates/:id/family", g.requireAuth, templateHandler.Family)
		// 派生链维护（F-3-04）：改的是同一条链，因此共用一个任务类型，
		// 下发时再各自映射到不同的 agent 操作。
		v1.POST("/templates/:id/rebase", g.requireAuth, templateHandler.RebaseTemplate)
		v1.POST("/templates/:id/flatten", g.requireAuth, templateHandler.FlattenTemplate)
		v1.POST("/templates/:id/promote-child", g.requireAuth, templateHandler.PromoteChildTemplate)
		v1.POST("/templates/:id/promote-delete", g.requireAuth, templateHandler.PromoteDeleteTemplate)
		// 离线预处理（F-3-06）：判定与执行都在节点侧，控制面只固化选项。
		v1.POST("/templates/:id/preprocess", g.requireAuth, g.adminOnly, templateHandler.Preprocess)

		// 模板导出与导入（F-3-05）：模板包是跨节点搬运模板的载体。
		//
		// 导出记在 /template-exports 下而不是 /templates/:id/exports：产物是
		// **独立的对象**（可以下载、可以删除、模板删了它还在），挂在模板下面
		// 会让"模板已删除"之后无处可找。
		v1.POST("/templates/:id/exports", g.requireAuth, templateHandler.Export)
		v1.GET("/template-exports", g.requireAuth, templateHandler.ListExports)
		v1.DELETE("/template-exports/:id", g.requireAuth, templateHandler.DeleteExport)
		v1.GET("/template-exports/:id/download", g.requireAuth, templateHandler.DownloadExport)
		v1.POST("/templates/imports/preview", g.requireAuth, templateHandler.ImportPreview)
		v1.POST("/templates/imports", g.requireAuth, templateHandler.Import)
		v1.DELETE("/templates/:id", g.requireAuth, templateHandler.Delete)

		// 跨节点迁移（F-2-09）。前置条件在受理时同步判定——每一个条件
		// 漏掉的代价都不是「操作失败」，而是两台宿主机上各留下一份不完整
		// 的东西。
		v1.POST("/vms/:id/migrate", g.requireAuth, vmHandler.Migrate)
		v1.GET("/vms/:id/migrations", g.requireAuth, vmHandler.Migrations)

		// 镜像导入（F-2-13）。
		//
		// 文件内容由前端**先分片上传**到「我的存储」（disk 类别，秒传与
		// 断点续传见 f-5-04），这里按文件名受理——受理与 Parse 都会校验
		// 文件确实已在该节点的存储中（G-34），没上传过的名字进不来。
		// 受理、状态流转、转换后建模板、配额记账由 importer 包完成。
		v1.GET("/imports/format", g.requireAuth, importerHandler.GuessFormat)
		v1.POST("/imports/parse", g.requireAuth, importerHandler.Parse)
		v1.GET("/imports", g.requireAuth, importerHandler.List)
		v1.POST("/imports", g.requireAuth, importerHandler.Create)
		v1.GET("/imports/:id", g.requireAuth, importerHandler.Get)
		v1.DELETE("/imports/:id", g.requireAuth, importerHandler.Delete)

		// 虚拟机标签（F-2-16）。
		//
		// 标签与分组解决的不是同一件事：分组互斥（一台机器只属于一个组），
		// 标签非互斥（可以有任意多个）。合并会立刻遇到矛盾——一台机器既是
		// 「生产」又是「数据库」，而分组只能放一个。
		//
		// 保存是**整体替换**而不是增量增删：界面上改完直接保存，而 add/remove
		// 会让「加了又删、删了又加」的中间态被如实写进审计流水。
		v1.GET("/tags", g.requireAuth, tagHandler.All)
		v1.GET("/tags/vms", g.requireAuth, tagHandler.VMsByTag)
		v1.GET("/vms/:id/tags", g.requireAuth, tagHandler.List)
		v1.PUT("/vms/:id/tags", g.requireAuth, tagHandler.Set)

		// 回收站（F-2-16）：删除只是把机器移出列表，彻底删除是这里的第二步。
		//
		// 三步里的前两步都**可逆**：移入回收站不动磁盘，恢复只是把记录放回
		// 列表。不可逆的（删盘 + 物理删记录）只发生在用户明确点「彻底删除」
		// 之后。
		v1.GET("/vms/trash", g.requireAuth, vmHandler.Trash)
		v1.POST("/vms/trash/:id/restore", g.requireAuth, vmHandler.Restore)
		v1.DELETE("/vms/trash/:id", g.requireAuth, vmHandler.Purge)

		// 计算资源配额（vCPU / 内存 / 实例数）。
		//
		// 它与 resource-quotas 是**两类约束**：后者按周期累计、超限后限速或
		// 断网；这里看的是"此刻占着多少"，超限的处置是**拒绝新建**——把一台
		// 正在跑的机器限速掉，比不让它再建一台严重得多。
		v1.GET("/compute-quotas", g.requireAuth, g.adminOnly, computeQuotaHandler.List)
		v1.PUT("/compute-quotas", g.requireAuth, g.adminOnly, computeQuotaHandler.Set)

		// PCIe 直通（GPU 等）。
		//
		// 归管理员：绑定设备会**改变宿主机上设备的归属**——把一块卡从宿主
		// 驱动抢过来，宿主就看不到它了。若那块卡正被宿主使用（例如唯一的
		// 网卡），绑定会让宿主机**当场失去网络**，而面板正是通过网络管理的。
		//
		// 挂载到虚拟机**要求关机**：直通设备不支持热插拔，而失败方式是
		// 一台机器卡在半启动状态。
		v1.GET("/host/passthrough", g.requireAuth, g.adminOnly, passthroughHandler.Overview)
		v1.POST("/host/passthrough/bind", g.requireAuth, g.adminOnly, passthroughHandler.Bind)
		v1.POST("/host/passthrough/unbind", g.requireAuth, g.adminOnly, passthroughHandler.Unbind)
		v1.GET("/vms/:id/passthrough", g.requireAuth, g.adminOnly, passthroughHandler.ListVM)
		v1.POST("/vms/:id/passthrough", g.requireAuth, g.adminOnly, passthroughHandler.Attach)
		v1.DELETE("/vms/:id/passthrough", g.requireAuth, g.adminOnly, passthroughHandler.Detach)

		// 来宾自动化（F-2-10）。四种动作共用一个入口。
		//
		// 不需要二次验证：改密与附加磁盘都**不是不可逆的数据操作**——
		// 密码可以再改回来，附加磁盘影响的是新盘（既有数据不受影响），
		// 扩容只是把盘变大。给可逆操作加验证只会稀释验证本身的分量。
		v1.GET("/vms/:id/guest-actions", g.requireAuth, vmHandler.GuestCapabilities)
		v1.POST("/vms/:id/guest-actions", g.requireAuth, vmHandler.GuestAction)

		// 导出（F-2-14）与产物下载。
		//
		// 都不需要二次验证：导出是只读地把系统盘打成镜像，删产物删的是一份
		// 副本——两者都不影响虚拟机本身。
		v1.GET("/vms/:id/exports", g.requireAuth, vmHandler.Exports)
		v1.POST("/vms/:id/exports", g.requireAuth, vmHandler.CreateExport)
		v1.DELETE("/vms/:id/exports/:exportID", g.requireAuth, vmHandler.DeleteExport)
		v1.GET("/vms/:id/exports/:exportID/download", g.requireAuth, vmHandler.DownloadExport)

		// 重装系统（F-2-11）。重建走二次验证（整块系统盘被替换，不可逆）；
		// 清理备份不需要——它删的是已不再被使用的备份，当前运行不受影响。
		v1.POST("/vms/:id/reinstall", g.requireAuth, vmHandler.Reinstall)
		v1.DELETE("/vms/:id/reinstall/backup", g.requireAuth, vmHandler.PurgeReinstallBackup)

		// 救援系统（F-2-12）。进入与退出都是任务：两者都要改虚拟机的硬件
		// 配置并重启，是宿主机上的实际操作。
		v1.POST("/vms/:id/rescue", g.requireAuth, vmHandler.EnterRescue)
		v1.DELETE("/vms/:id/rescue", g.requireAuth, vmHandler.ExitRescue)

		// Hero 的资源卡与控制台预览卡（f-2-01 §5.3.3）。
		//
		// 两者都是**只读探测**，不入队、不写投影：指标与画面都是瞬时的，
		// 存下来只会在下一次读取时给出一个过期的答案。
		v1.GET("/vms/:id/stats", g.requireAuth, vmHandler.Stats)
		// 详情页的三块补充能力：事件时间线、PCIe 槽位余量、邻居表。三者都是**按需**取——它们各需要一次向节点的请求或一次流水查询，塞进详情会让每次打开详情页都变慢。
		v1.GET("/vms/:id/timeline", g.requireAuth, vmHandler.Timeline)
		v1.GET("/vms/:id/pcie-info", g.requireAuth, vmHandler.PCIeInfo)
		v1.GET("/vms/:id/neighbors", g.requireAuth, vmHandler.Neighbors)
		v1.GET("/vms/:id/console/frame", g.requireAuth, vmHandler.ConsoleFrame)
		// 电源与删除都是异步操作：受理时校验状态并返回任务标识，执行由
		// 任务队列按资源锁串行（f-2-01 R-005）。
		v1.POST("/vms/:id/power-actions", g.requireAuth, vmHandler.Power)
		v1.DELETE("/vms/:id", g.requireAuth, vmHandler.Delete)
		// 强制删除（僵尸机兜底）：不探测状态、节点侧删域定义并重启
		// libvirt。管理员 + 二次验证（handler 内）双守卫——影响面不止
		// 这一台机器。
		v1.POST("/vms/:id/force-delete", g.requireAuth, g.adminOnly, vmHandler.ForceDelete)
		// 分配归属（F-1-07）：把已有虚拟机指派给某个用户。仅管理员。
		v1.PUT("/vms/:id/owner", g.requireAuth, vmHandler.AssignOwner)

		// 详情页「网络管理」标签页（F-2-03）。读接口直接返回投影；
		// 写接口全部入队——它们都要下发到节点，且资源锁与电源操作共用
		// vm:<id>，因此不会出现「改完网卡正好赶上关机」。
		v1.GET("/vms/:id/interfaces", g.requireAuth, vmHandler.Interfaces)
		v1.GET("/vms/:id/static-ips", g.requireAuth, vmHandler.StaticIPs)
		v1.POST("/vms/:id/interfaces", g.requireAuth, vmHandler.AddInterface)
		v1.PATCH("/vms/:id/interfaces/:nicID", g.requireAuth, vmHandler.UpdateInterface)
		v1.DELETE("/vms/:id/interfaces/:nicID", g.requireAuth, vmHandler.RemoveInterface)
		v1.POST("/vms/:id/static-ips", g.requireAuth, vmHandler.BindStaticIP)
		v1.DELETE("/vms/:id/static-ips/:ipID", g.requireAuth, vmHandler.UnbindStaticIP)
		// 批量删除端口转发。逐条调用单条方法，复用全部校验（尤其是归属）。
		// 注册在 `:id` 路由之前，理由同上。
		v1.POST("/vms/port-forwards/batch-delete", g.requireAuth, vmHandler.BatchRemovePortForwards)
		v1.GET("/vms/:id/port-forwards", g.requireAuth, vmHandler.PortForwards)
		v1.POST("/vms/:id/port-forwards", g.requireAuth, vmHandler.AddPortForward)
		v1.DELETE("/vms/:id/port-forwards/:pfID", g.requireAuth, vmHandler.RemovePortForward)

		// 编辑配置（F-2-05）。元数据与硬件分开：前者是纯控制面数据，
		// 同步改库即可；后者要下发到节点，走任务队列。
		v1.GET("/vms/:id/edit-form", g.requireAuth, vmHandler.EditForm)
		v1.PATCH("/vms/:id/metadata", g.requireAuth, vmHandler.UpdateMetadata)
		v1.POST("/vms/:id/config-changes", g.requireAuth, vmHandler.UpdateConfig)

		// 快照（F-2-07）。三个动作全部走任务队列：创建与恢复要复制或回滚
		// 整个磁盘镜像，同步等待必然超时（f-7-01 R-001）。
		v1.GET("/vms/:id/snapshots", g.requireAuth, vmHandler.Snapshots)
		v1.POST("/vms/:id/snapshots", g.requireAuth, vmHandler.CreateSnapshot)
		v1.POST("/vms/:id/snapshots/:snapshotID/restore", g.requireAuth, vmHandler.RestoreSnapshot)
		v1.DELETE("/vms/:id/snapshots/:snapshotID", g.requireAuth, vmHandler.DeleteSnapshot)
		// 删除全部快照（F-2-07）。受**二次验证**保护：丢一个还原点与丢掉
		// 整条时间线不是同一件事，而点这个按钮的人多半没有逐条确认过。
		v1.POST("/vms/:id/snapshots/delete-all", g.requireAuth, vmHandler.DeleteAllSnapshots)
		// UEFI 启动项修复（F-2-11）。恢复快照之后的典型后果就是它。
		v1.POST("/vms/:id/nvram/repair", g.requireAuth, vmHandler.RepairNVRAM)

		// 定时任务（F-7-05）。执行时复用已有的 vm.power / vm.delete 任务，
		// 因此不需要新的执行器。删除类任务在**创建时**走二次验证——它是
		// 一条将来会自动执行的删除指令，留到执行时再验证就没人可验了。
		v1.GET("/vms/:id/schedules", g.requireAuth, scheduleHandler.List)
		v1.POST("/vms/:id/schedules", g.requireAuth, scheduleHandler.Create)
		v1.PATCH("/vms/:id/schedules/:scheduleID", g.requireAuth, scheduleHandler.SetEnabled)
		v1.DELETE("/vms/:id/schedules/:scheduleID", g.requireAuth, scheduleHandler.Delete)

		// 控制台（F-2-08）。WebSocket 端点同样经过认证中间件：Cookie 随
		// 握手请求发送，因此**升级前**就完成了鉴权与授权（R-003）——
		// 升级之后没有 HTTP 状态码可用，那时再拒绝已经没有合适的表达方式。
		//
		// 「对外暴露」是受二次验证保护的高危操作，由 handler 按请求内容
		// 决定是否要求验证（只改暴露状态时才要求）。
		v1.GET("/vms/:id/console", g.requireAuth, consoleHandler.GetConfig)
		v1.PATCH("/vms/:id/console", g.requireAuth, consoleHandler.Update)
		v1.GET("/vms/:id/console/screenshot", g.requireAuth, consoleHandler.Screenshot)
		// G-30：登录凭据读取。明文出站，审计在服务层写入。
		v1.GET("/vms/:id/initial-credential", g.requireAuth, vmHandler.InitialCredential)
		v1.GET("/vms/:id/console/ws", g.requireAuth, consoleHandler.WS)
		// SPICE 连接文件（.vv）。含明文密码的版本需要二次验证，由 handler
		// 按查询参数决定——默认不含密码，用户手输即可。
		v1.GET("/vms/:id/console/connection-file", g.requireAuth, consoleHandler.ConnectionFile)
		// 虚拟机定义（**只读**）。排查「面板显示的和实际跑的不是一回事」时
		// 它是唯一的真相，因此总是现读、不缓存。返回内容已脱敏——libvirt
		// 的定义里有控制台密码，原样送出等于把界面上「只写不读」的凭据
		// 从后门送出去。
		v1.GET("/vms/:id/xml", g.requireAuth, consoleHandler.XML)
		// 关机状态下的磁盘扩容（**只能扩，不能缩**）。运行中的扩容走
		// 「来宾自动化」里的 expand_disk——那条路会顺带在来宾里扩好文件系统。
		v1.POST("/vms/:id/disk/resize", g.requireAuth, vmHandler.ResizeDisk)
		// 磁盘管理（F-2-06）：列表为实时探测，变更为入队操作。
		//
		// 三件事刻意分开成三种资源语义：扩容只能扩（缩容必坏）、卸载不能
		// 碰系统盘、换总线必须关机。它们各自的约束差别很大，合并成一个
		// 「改磁盘」接口会把这些约束挤成一句笼统的"不允许"。
		v1.GET("/vms/:id/disks", g.requireAuth, vmHandler.Disks)
		v1.POST("/vms/:id/disks", g.requireAuth, vmHandler.ChangeDisk)
		// 把链接克隆的磁盘变为独立盘（**需要停机**）。它存在的理由是
		// 链接克隆的父盘删不掉，而模板的管理需要能删掉旧的父盘。
		v1.POST("/vms/:id/disks/independent", g.requireAuth, vmHandler.MakeDisksIndependent)
		// 批量克隆（一次最多 5 台）。限制与存储 IO 有关，理由写在报错里。
		v1.POST("/vms/batch-clone", g.requireAuth, vmHandler.BatchClone)

		// 迁移预检（**只读**）。它回答用户点下按钮之前唯一想知道的那件事：
		// 这次要停多久。因此除了「能不能迁」，还给出「会怎么迁」——
		// 停机时长由后者决定。**与迁移共用同一套校验**。
		v1.POST("/vms/:id/migration/preview", g.requireAuth, vmHandler.PreviewMigration)
		// 迁移目标清单（F-6-03 / F-6-04，只读）：按节点聚合容量采样与
		// 冲突检查，并给出推荐。容量来自最近一次采样而不是实时探测。
		v1.GET("/vms/:id/migrate-targets", g.requireAuth, vmHandler.MigrateTargets)

		// 光驱（可以有多个）。**弹出与移除是两件事**：弹出之后光驱仍在
		// （来宾里看得到一个空的托盘），移除才是设备消失。
		// **换盘之后来宾通常看不到新介质**（多数系统缓存了介质信息），
		// 界面要提示可以重新挂载或重启；**换总线通常需要重启**。
		v1.GET("/vms/:id/cdroms", g.requireAuth, vmHandler.CDROMs)
		v1.POST("/vms/:id/cdroms", g.requireAuth, vmHandler.AttachCDROM)
		v1.PUT("/vms/:id/cdroms/:cdromID/iso", g.requireAuth, vmHandler.LoadCDROM)
		v1.POST("/vms/:id/cdroms/:cdromID/eject", g.requireAuth, vmHandler.EjectCDROM)
		v1.PUT("/vms/:id/cdroms/:cdromID/bus", g.requireAuth, vmHandler.SetCDROMBus)
		v1.DELETE("/vms/:id/cdroms/:cdromID", g.requireAuth, vmHandler.RemoveCDROM)
		// 校验并给出 diff（**只读**，不需要二次验证——不产生改动，而
		// "看一眼会影响什么"如果需要先验证一次，用户就会在还不知道要改
		// 什么的时候被迫走一遍验证流程）。
		v1.POST("/vms/:id/xml/precheck", g.requireAuth, consoleHandler.XMLPrecheck)
		// 应用。**需要二次验证**：它绕过我们建立的其它全部校验。
		v1.PUT("/vms/:id/xml", g.requireAuth, consoleHandler.XMLUpdate)
	}
}
