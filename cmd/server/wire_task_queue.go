// wire_task_queue 装配任务队列（internal/ops/task）与实时总线
// （internal/ops/realtime），并注册全部领域执行器。
//
// 执行器注册的顺序无语义（队列按任务类型查表派发），但按「域内聚」排布，
// 与 internal/ 各域的阅读顺序一致。createExec / guestExec 单独留在
// a.compute 上：它们要在 setupCredentials 里补注入加密密钥，而那发生在
// 队列注册之后。
package main

import (
	"context"

	"k_cockpit/internal/compute/importer"
	"k_cockpit/internal/compute/passthrough"
	"k_cockpit/internal/compute/template"
	"k_cockpit/internal/compute/vm"
	"k_cockpit/internal/network/capture"
	"k_cockpit/internal/network/hostfirewall"
	"k_cockpit/internal/network/portsecurity"
	"k_cockpit/internal/network/publicip"
	"k_cockpit/internal/network/securitygroup"
	"k_cockpit/internal/network/vpcacl"
	"k_cockpit/internal/network/vswitch"
	"k_cockpit/internal/ops/hosttuning"
	"k_cockpit/internal/ops/platformcheck"
	"k_cockpit/internal/ops/quotaenforce"
	"k_cockpit/internal/ops/realtime"
	"k_cockpit/internal/ops/task"
	"k_cockpit/internal/storage/pool"
)

// setupTaskQueue 建实时总线与任务队列，注册全部执行器并启动调度循环。
func (a *app) setupTaskQueue() {
	db, mockAgent := a.db, a.mockAgent

	// 实时事件总线：任务状态变化由队列在关键节点广播，SSE 端点订阅它。
	//
	// 先建总线再建队列，是因为队列要在入队、派发与落定三处发布事件——
	// 顺序反了就只能事后补一次装配，而那种"可选装配"最容易被忘记。
	a.bus = realtime.NewBus()
	// 任务队列：所有异步操作的载体。注册各能力的 Executor，队列本身
	// 不关心任务具体做什么——新增能力时只需在这里多注册一个。
	a.queue = task.NewQueue(db, a.recorder, task.Options{}).WithBus(a.bus)

	// --- compute 域执行器 ---
	a.compute.createExec = vm.NewCreateExecutor(db, mockAgent)
	a.queue.Register(a.compute.createExec)
	a.queue.Register(vm.NewPowerExecutor(db, mockAgent))
	a.queue.Register(vm.NewDeleteExecutor(db, mockAgent))
	// 快照（F-2-07）。三个执行器共用资源锁键 vm:<id>，因此与电源操作天然
	// 互斥——恢复快照时不会有并发的开机请求插进来。
	a.queue.Register(vm.NewSnapshotCreateExecutor(db, mockAgent))
	a.queue.Register(vm.NewSnapshotRestoreExecutor(db, mockAgent))
	a.queue.Register(vm.NewSnapshotDeleteExecutor(db, mockAgent))
	// 批量删除快照与 UEFI 启动项修复（F-2-07 / F-2-11）。
	a.queue.Register(vm.NewSnapshotDeleteAllExecutor(db, mockAgent))
	a.queue.Register(vm.NewNVRAMRepairExecutor(mockAgent))
	a.queue.Register(vm.NewConfigUpdateExecutor(db, mockAgent))
	a.queue.Register(vm.NewEnterRescueExecutor(db, mockAgent))
	a.queue.Register(vm.NewExitRescueExecutor(db, mockAgent))
	// 重装要备份并重建系统盘，同样是磁盘操作。
	a.queue.Register(vm.NewReinstallExecutor(db, mockAgent))
	a.queue.Register(vm.NewPurgeExecutor(db, mockAgent))
	// 导出要打包整块磁盘，可能跑到几十分钟。
	a.queue.Register(vm.NewExportExecutor(db, mockAgent))
	a.queue.Register(vm.NewExportDeleteExecutor(db, mockAgent))
	// 来宾自动化要进系统内部执行，同样是异步的。
	// （改密成功后要同步凭据记录，需要加密密钥——在 setupCredentials 处注入。）
	a.compute.guestExec = vm.NewGuestExecutor(db, mockAgent)
	a.queue.Register(a.compute.guestExec)
	// 迁移要搬运整块磁盘，是最耗时的操作之一。
	a.queue.Register(vm.NewMigrateExecutor(db, mockAgent))
	// 关机状态下的磁盘扩容。
	a.queue.Register(vm.NewDiskResizeExecutor(db, mockAgent))
	// 磁盘的挂载 / 卸载 / 换总线（F-2-06）。
	a.queue.Register(vm.NewDiskChangeExecutor(db, mockAgent))
	// 链接克隆的磁盘合并为独立镜像。
	a.queue.Register(vm.NewIndependentExecutor(db, mockAgent))
	// 光驱（挂载 / 弹出 / 换盘 / 摘除 / 换总线）。
	a.queue.Register(vm.NewCDROMExecutor(db, mockAgent))
	// 网络变更（F-2-03）：三种资源各一个执行器，共用 vm:<id> 资源锁。
	a.queue.Register(vm.NewInterfaceChangeExecutor(db, mockAgent))
	a.queue.Register(vm.NewStaticIPChangeExecutor(db, mockAgent))
	a.queue.Register(vm.NewPortForwardChangeExecutor(db, mockAgent))
	// 模板制备要复制整块系统盘，因此与其它磁盘操作一样走队列。
	a.queue.Register(template.NewPrepareExecutor(db, mockAgent))
	a.queue.Register(template.NewDeleteExecutor(db, mockAgent))
	// 模板导出与导入（F-3-05）：打包与解包都要读写整块镜像。
	a.queue.Register(template.NewExportExecutor(db, mockAgent))
	a.queue.Register(template.NewExportDeleteExecutor(db, mockAgent))
	a.queue.Register(template.NewImportExecutor(db, mockAgent))
	// 派生链维护（rebase / 拉平 / 提升 / 热提升删除）。
	a.queue.Register(template.NewMaintainExecutor(db, mockAgent))
	// 离线预处理（F-3-06）。
	a.queue.Register(template.NewPreprocessExecutor(db, mockAgent))
	// 镜像导入要转换格式，可能处理几十 GB 的文件。
	a.queue.Register(importer.NewImportExecutor(db, mockAgent))
	// PCIe 直通设备的挂载与卸载。
	a.queue.Register(passthrough.NewExecutor(db, mockAgent))

	// --- network 域执行器 ---
	// ACL 应用（F-4-05）。
	a.queue.Register(vpcacl.NewExecutor(mockAgent))
	// 公网地址变更要动宿主机的 iptables 与路由。
	a.queue.Register(publicip.NewChangeExecutor(db, mockAgent))
	// 安全组规则要写进宿主机运行域的规则链。
	a.queue.Register(securitygroup.NewApplyExecutor(db, mockAgent))
	// 端口安全要往节点流表里写规则。
	a.queue.Register(portsecurity.NewExecutor(db, mockAgent))
	// 抓包：一次限时的抓包，以及删除节点上的抓包文件。
	a.queue.Register(capture.NewExecutor(db, mockAgent))
	a.queue.Register(capture.NewDeleteExecutor(db, mockAgent))
	// 宿主机防火墙：应用与紧急回滚。
	a.queue.Register(hostfirewall.NewExecutor(db, mockAgent))
	// 交换机变更要建网桥，因此与存储池一样走队列。
	a.queue.Register(vswitch.NewSwitchChangeExecutor(db, mockAgent))

	// --- storage 域执行器 ---
	// 目录共享要往虚拟机的域配置里加一块 virtio-9p 设备。
	a.queue.Register(pool.NewShareExecutor(db, mockAgent))
	// 存储卷要跑 pvcreate/vgcreate/lvcreate，删卷还要逆序释放设备。
	a.queue.Register(pool.NewVolumeExecutor(db, mockAgent))
	a.queue.Register(pool.NewCreateExecutor(db, mockAgent))
	a.queue.Register(pool.NewDeleteExecutor(db, mockAgent))
	// 分区、池配置与卸载（F-5-01 后续迭代）。
	a.queue.Register(pool.NewPartitionExecutor(mockAgent))
	a.queue.Register(pool.NewPartitionDeleteExecutor(mockAgent))
	a.queue.Register(pool.NewPoolConfigExecutor(db, mockAgent))
	a.queue.Register(pool.NewPoolUnmountExecutor(db, mockAgent))

	// --- ops 域执行器 ---
	// 配额处置：对某用户的网络施加或撤销限速 / 断网。
	a.queue.Register(quotaenforce.NewExecutor(db, mockAgent))
	// 宿主机性能调优。
	a.queue.Register(hosttuning.NewExecutor(db, mockAgent))
	// 平台自检后的重新下发。
	a.queue.Register(platformcheck.NewExecutor(db, mockAgent))

	a.queue.Start(context.Background())
}
