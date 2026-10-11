// wire_task_queue 装配任务队列（internal/service/ops/task）与实时总线
// （internal/service/ops/realtime），并注册全部领域执行器。
//
// 执行器注册的顺序无语义（队列按任务类型查表派发），但按「域内聚」排布，
// 与 internal/ 各域的阅读顺序一致。createExec / guestExec 单独留在
// a.compute 上：它们要在 setupCredentials 里补注入加密密钥，而那发生在
// 队列注册之后。
package main

import (
	"context"

	"k_cockpit/internal/service/compute/importer"
	"k_cockpit/internal/service/compute/passthrough"
	"k_cockpit/internal/service/compute/template"
	"k_cockpit/internal/service/compute/vm"
	"k_cockpit/internal/service/network/capture"
	"k_cockpit/internal/service/network/hostfirewall"
	"k_cockpit/internal/service/network/portsecurity"
	"k_cockpit/internal/service/network/publicip"
	"k_cockpit/internal/service/network/securitygroup"
	"k_cockpit/internal/service/network/vpcacl"
	"k_cockpit/internal/service/network/vswitch"
	"k_cockpit/internal/service/ops/hosttuning"
	"k_cockpit/internal/service/ops/platformcheck"
	"k_cockpit/internal/service/ops/quotaenforce"
	"k_cockpit/internal/service/ops/realtime"
	"k_cockpit/internal/service/ops/task"
	"k_cockpit/internal/service/storage/pool"
)

// setupTaskQueue 建实时总线与任务队列，注册全部执行器并启动调度循环。
func (a *app) setupTaskQueue() {
	db, agentClient := a.db, a.agentClient

	// 实时事件总线：任务状态变化由队列在关键节点广播，SSE 端点订阅它。
	//
	// 先建总线再建队列，是因为队列要在入队、派发与落定三处发布事件——
	// 顺序反了就只能事后补一次装配，而那种"可选装配"最容易被忘记。
	a.bus = realtime.NewBus()
	// 任务队列：所有异步操作的载体。注册各能力的 Executor，队列本身
	// 不关心任务具体做什么——新增能力时只需在这里多注册一个。
	a.queue = task.NewQueue(db, a.recorder, task.Options{}).WithBus(a.bus)

	// --- compute 域执行器 ---
	a.compute.createExec = vm.NewCreateExecutor(db, agentClient)
	a.queue.Register(a.compute.createExec)
	a.queue.Register(vm.NewPowerExecutor(db, agentClient))
	a.queue.Register(vm.NewDeleteExecutor(db, agentClient))
	// 快照（F-2-07）。三个执行器共用资源锁键 vm:<id>，因此与电源操作天然
	// 互斥——恢复快照时不会有并发的开机请求插进来。
	a.queue.Register(vm.NewSnapshotCreateExecutor(db, agentClient))
	a.queue.Register(vm.NewSnapshotRestoreExecutor(db, agentClient))
	a.queue.Register(vm.NewSnapshotDeleteExecutor(db, agentClient))
	// 批量删除快照与 UEFI 启动项修复（F-2-07 / F-2-11）。
	a.queue.Register(vm.NewSnapshotDeleteAllExecutor(db, agentClient))
	a.queue.Register(vm.NewNVRAMRepairExecutor(agentClient))
	a.queue.Register(vm.NewConfigUpdateExecutor(db, agentClient))
	a.queue.Register(vm.NewEnterRescueExecutor(db, agentClient))
	a.queue.Register(vm.NewExitRescueExecutor(db, agentClient))
	// 重装要备份并重建系统盘，同样是磁盘操作。
	a.queue.Register(vm.NewReinstallExecutor(db, agentClient))
	a.queue.Register(vm.NewPurgeExecutor(db, agentClient))
	// 导出要打包整块磁盘，可能跑到几十分钟。
	a.queue.Register(vm.NewExportExecutor(db, agentClient))
	a.queue.Register(vm.NewExportDeleteExecutor(db, agentClient))
	// 来宾自动化要进系统内部执行，同样是异步的。
	// （改密成功后要同步凭据记录，需要加密密钥——在 setupCredentials 处注入。）
	a.compute.guestExec = vm.NewGuestExecutor(db, agentClient)
	a.queue.Register(a.compute.guestExec)
	// 迁移要搬运整块磁盘，是最耗时的操作之一。
	a.queue.Register(vm.NewMigrateExecutor(db, agentClient))
	// 关机状态下的磁盘扩容。
	a.queue.Register(vm.NewDiskResizeExecutor(db, agentClient))
	// 磁盘的挂载 / 卸载 / 换总线（F-2-06）。
	a.queue.Register(vm.NewDiskChangeExecutor(db, agentClient))
	// 链接克隆的磁盘合并为独立镜像。
	a.queue.Register(vm.NewIndependentExecutor(db, agentClient))
	// 光驱（挂载 / 弹出 / 换盘 / 摘除 / 换总线）。
	a.queue.Register(vm.NewCDROMExecutor(db, agentClient))
	// 网络变更（F-2-03）：三种资源各一个执行器，共用 vm:<id> 资源锁。
	a.queue.Register(vm.NewInterfaceChangeExecutor(db, agentClient))
	a.queue.Register(vm.NewStaticIPChangeExecutor(db, agentClient))
	a.queue.Register(vm.NewPortForwardChangeExecutor(db, agentClient))
	// 模板制备要复制整块系统盘，因此与其它磁盘操作一样走队列。
	a.queue.Register(template.NewPrepareExecutor(db, agentClient))
	a.queue.Register(template.NewDeleteExecutor(db, agentClient))
	// 模板导出与导入（F-3-05）：打包与解包都要读写整块镜像。
	a.queue.Register(template.NewExportExecutor(db, agentClient))
	a.queue.Register(template.NewExportDeleteExecutor(db, agentClient))
	a.queue.Register(template.NewImportExecutor(db, agentClient))
	// 派生链维护（rebase / 拉平 / 提升 / 热提升删除）。
	a.queue.Register(template.NewMaintainExecutor(db, agentClient))
	// 离线预处理（F-3-06）。
	a.queue.Register(template.NewPreprocessExecutor(db, agentClient))
	// 镜像导入要转换格式，可能处理几十 GB 的文件。
	a.queue.Register(importer.NewImportExecutor(db, agentClient))
	// PCIe 直通设备的挂载与卸载。
	a.queue.Register(passthrough.NewExecutor(db, agentClient))

	// --- network 域执行器 ---
	// ACL 应用（F-4-05）。
	a.queue.Register(vpcacl.NewExecutor(agentClient))
	// 公网地址变更要动宿主机的 iptables 与路由。
	a.queue.Register(publicip.NewChangeExecutor(db, agentClient))
	// 安全组规则要写进宿主机运行域的规则链。
	a.queue.Register(securitygroup.NewApplyExecutor(db, agentClient))
	// 端口安全要往节点流表里写规则。
	a.queue.Register(portsecurity.NewExecutor(db, agentClient))
	// 抓包：一次限时的抓包，以及删除节点上的抓包文件。
	a.queue.Register(capture.NewExecutor(db, agentClient))
	a.queue.Register(capture.NewDeleteExecutor(db, agentClient))
	// 宿主机防火墙：应用与紧急回滚。
	a.queue.Register(hostfirewall.NewExecutor(db, agentClient))
	// 交换机变更要建网桥，因此与存储池一样走队列。
	a.queue.Register(vswitch.NewSwitchChangeExecutor(db, agentClient))

	// --- storage 域执行器 ---
	// 目录共享要往虚拟机的域配置里加一块 virtio-9p 设备。
	a.queue.Register(pool.NewShareExecutor(db, agentClient))
	// 存储卷要跑 pvcreate/vgcreate/lvcreate，删卷还要逆序释放设备。
	a.queue.Register(pool.NewVolumeExecutor(db, agentClient))
	a.queue.Register(pool.NewCreateExecutor(db, agentClient))
	a.queue.Register(pool.NewDeleteExecutor(db, agentClient))
	// 分区、池配置与卸载（F-5-01 后续迭代）。
	a.queue.Register(pool.NewPartitionExecutor(agentClient))
	a.queue.Register(pool.NewPartitionDeleteExecutor(agentClient))
	a.queue.Register(pool.NewPoolConfigExecutor(db, agentClient))
	a.queue.Register(pool.NewPoolUnmountExecutor(db, agentClient))

	// --- ops 域执行器 ---
	// 配额处置：对某用户的网络施加或撤销限速 / 断网。
	a.queue.Register(quotaenforce.NewExecutor(db, agentClient))
	// 宿主机性能调优。
	a.queue.Register(hosttuning.NewExecutor(db, agentClient))
	// 平台自检后的重新下发。
	a.queue.Register(platformcheck.NewExecutor(db, agentClient))

	a.queue.Start(context.Background())
}
