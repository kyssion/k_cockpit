// Package agent 定义控制面与节点 agent 的交互契约。
//
// 控制面不直连宿主机（[ADR-0005]）：全部宿主侧动作都经本包下达。
//
// 本期**不开发 agent 侧**：接口只保留契约，由 mock 直接返回结果（[ADR-0007]）。
// 业务代码只依赖 Client，不知道背后是 mock 还是 gRPC，因此换成节点侧实现时
// 只需替换 Client，调用方无需改动。
//
// 接口**按需扩展**：实现每个业务能力时，才向 Client 添加该能力所需的方法，
// 不为尚未实现的能力臆测协议形态。相关决策见 docs/06-decisions/ 的 ADR-0005
// 与 ADR-0007。
package agent

import "context"

// OpKind 是领域操作的类型标识，命名遵循「资源域.动作」。
type OpKind string

// 领域操作标识。
//
// 按需扩展：新增能力时在此登记，并在 Executor 中实现对应的执行逻辑。
const (
	// OpVMStatus 探测虚拟机运行态。**只读**，不改变任何东西。
	//
	// 它存在的理由是 f-2-01 R-002：投影字段不得参与业务判定——投影可能滞后，
	// 凭它判断「已关机」就去执行删除，可能在虚拟机实际运行时执行危险操作。
	// 因此写操作前必须探测真实状态。
	OpVMStatus OpKind = "vm.status"

	OpVMCreate   OpKind = "vm.create"
	OpVMStart    OpKind = "vm.start"
	OpVMShutdown OpKind = "vm.shutdown"
	// OpVMPoweroff 是强制断电，与 OpVMShutdown 是**两个独立操作**：
	// 静默强杀可能造成来宾文件系统损坏，何时放弃等待应由用户判断，
	// 因此不做超时自动降级（f-2-01 R-006）。
	OpVMPoweroff OpKind = "vm.poweroff"
	OpVMReboot   OpKind = "vm.reboot"
	// OpVMReset 是硬重置，仅对暂停态可用（f-2-01 R-007）。
	OpVMReset  OpKind = "vm.reset"
	OpVMDelete OpKind = "vm.delete"

	// 快照操作（F-2-07）。三个动作**都是耗时操作**：创建与恢复要复制或
	// 回滚整个磁盘镜像，因此全部走任务队列，接口不同步等待。
	OpVMSnapshotCreate  OpKind = "vm.snapshot.create"
	OpVMSnapshotRestore OpKind = "vm.snapshot.restore"
	OpVMSnapshotDelete  OpKind = "vm.snapshot.delete"
	// OpVMNVRAMRepairResultKey 是 NVRAM 修复结果的键。
	OpVMNVRAMRepairResultKey = "nvram"

	// OpVMNVRAMRepair 修复 UEFI 启动项（F-2-11）。
	//
	// 恢复快照之后，UEFI 固件里记录的启动项可能仍指向已经不存在的磁盘
	// 或文件路径，表现为"开机进不了系统、直接进 UEFI Shell"。这不是磁盘
	// 坏了，只是固件里的那一条记录过期了——修的是那一小段，不是整块盘。
	OpVMNVRAMRepair OpKind = "vm.nvram.repair"

	// OpVMConfigUpdate 修改虚拟机硬件配置（F-2-05）。
	//
	// 只有需要下发到节点的改动才走这里：备注、分组是纯控制面元数据，
	// 虚拟化层不知道它们的存在。
	OpVMConfigUpdate OpKind = "vm.config.update"

	// 网络变更（F-2-03）。三者都是「把控制面的期望状态同步到节点」，
	// 因此参数形状一致：action + 资源的完整描述。
	OpVMInterfaceChange   OpKind = "vm.interface.change"
	OpVMStaticIPChange    OpKind = "vm.staticip.change"
	OpVMPortForwardChange OpKind = "vm.portforward.change"

	// OpVMStats 读取虚拟机的运行指标（CPU / 内存 / 网络 / 磁盘 / 运行时长）。
	//
	// 它是**只读探测**，与 OpVMStatus 同类：不入队、不写投影，只在被请求时
	// 向节点取一次。指标必须来自这里而不是控制面推算——控制面看到的 vcpu
	// 与 memory_mb 是**配置**，不是**用量**；把配置当用量显示，用户会看到
	// 一台空闲机器常年「内存占满」。
	//
	// uptime 也由节点给出：只有虚拟化层知道域是什么时候真正起来的，而
	// 控制面记录的「上次开机成功时间」会因重启、快照恢复等原因与实际不符。
	OpVMStats OpKind = "vm.stats"

	// OpVMRescueEnter / OpVMRescueExit 进入与退出救援模式（F-2-12）。
	//
	// 两个独立操作而不是一个带 action 的：它们各自要重启虚拟机、各自可能
	// 失败，合并会让「进入成功了但退出失败」这种情况无法分别重试。
	//
	// 两者的参数都带**配置快照**：进入时节点据此知道原配置是什么（救援档案
	// 要在此基础上调整），退出时据此还原。把快照交给节点、而不是让节点
	// 自己记忆——节点重启或重装后就再也说不出「原来是怎样」了。
	OpVMRescueEnter OpKind = "vm.rescue.enter"
	OpVMRescueExit  OpKind = "vm.rescue.exit"

	// OpVMConsoleFrame 抓取一帧控制台画面（f-2-01 的 Hero 控制台预览卡）。
	//
	// 与 OpVMStatus 一样是只读的，但它**较慢**（要等 hypervisor 出一帧），
	// 因此界面按固定间隔（20s）轮询，而不是随页面刷新。
	OpVMConsoleFrame OpKind = "vm.console.frame"
)

// StatsDataKey 是 OpVMStats 结果中承载指标的键。
const StatsDataKey = "stats"

// FrameDataKey 是 OpVMConsoleFrame 结果中承载画面数据的键。
const FrameDataKey = "frame"

// VMStats 是节点上报的一台虚拟机的运行指标。
//
// 各字段的单位写进名字里（MB / Kbps / Seconds）：一个叫 `mem` 的字段到底是
// 字节、KB 还是 MB，只有写它的人知道，而读它的人只能去翻实现——单位错误
// 不会报错，只会让界面把一个数量级错误的数字显示得很正常。
type VMStats struct {
	// CPUPercent 是相对**全部 vCPU** 的占用率（0-100）。
	//
	// 不用「单核百分比」：一台 4 核机器跑满一个核时，单核口径会显示 100%，
	// 而用户看到 100% 的第一反应是「机器满载了」。
	CPUPercent float64

	MemTotalMB int
	MemUsedMB  int

	NetRxKbps float64
	NetTxKbps float64
	// DiskReadKbps / DiskWriteKbps 是磁盘吞吐。
	//
	// 用吞吐而不是 IOPS（规格里另有 IOPS **限制**的配置项，那是上限不是用量）：
	// 用户看「这台机器在忙什么」时，吞吐比 IOPS 更好理解，而 IOPS 上限的
	// 实际效果需要配合队列深度才有意义。
	DiskReadKbps  float64
	DiskWriteKbps float64

	// UptimeSeconds 是域已经运行的时间；0 表示未运行。
	UptimeSeconds int64
}

// ConsoleFrame 是一帧控制台画面。
type ConsoleFrame struct {
	// MIME 是画面的格式，当前为 image/png。
	MIME string
	// Data 是**原始字节**，由调用方决定如何编码（本项目在接口层转 base64）。
	//
	// 不在协议层就转 base64：那会让每一层都以为「画面本来就是字符串」，
	// 而它实际是二进制——将来换成 JPEG 或流式传输时，这个假设会变成障碍。
	Data []byte
	// Width / Height 供界面预留位置，避免画面加载完成时布局跳动。
	Width  int
	Height int
}

// StatusDataKey 是 OpVMStatus 结果中承载运行态的键。
//
// 探测结果经 Data 传递而不新增方法：Client 的每次调用都应被理解为
// 「向节点下一个指令」，探测同样是一次指令往返。
const StatusDataKey = "status"

// Operation 描述一次要节点执行的领域操作。
type Operation struct {
	Kind   OpKind         // 操作类型
	NodeID int64          // 目标节点
	Target string         // 目标资源标识，按 Kind 解释（如虚拟机名、设备路径）
	Params map[string]any // 操作参数

	// OnStage 由**节点侧**在进入一个新阶段时调用，按实际发生的顺序。
	//
	// 为什么阶段必须来自节点：控制面并不知道一次创建在宿主机上分了几步、
	// 每步各花了多久。自己按操作类型编一条时间线，在换成节点侧实现后会立刻
	// 对不上号——而一条「看起来对、其实不对」的时间线比没有更糟，它会让人
	// 按错误的信息去定位问题。
	//
	// 调用方把它接到 task 包的阶段记录器上；为 nil 表示这次调用不关心阶段
	// （如探测类操作），实现方必须容忍 nil。
	//
	// 实现方**不需要**保证阶段完整或成对：调用方按「开始即记录、下一个开始
	// 时收尾上一个」处理，因此漏报或重复报都不会让记录出错。
	OnStage func(Stage)
}

// Stage 是节点上报的一个执行阶段。
type Stage struct {
	// Key 是稳定标识（供程序判断），Name 是中文名（供界面显示）。
	Key  string
	Name string
	// Index 是当前步骤序号，从 1 开始。
	Index int
	// Total 是节点**预计**的总步数，用于换算进度。
	//
	// 为 0 表示节点不知道总数（节点在执行前未必能给出完整计划）。
	// 此时进度留空由界面显示为「进行中」，而不是按已完成的步数硬凑一个
	// 百分比——一个从 25% 直接跳到 100% 的进度条，看起来像卡住后突然完成。
	Total int
}

// Result 是操作结果。
//
// Success 表示宿主侧是否成功完成；Message 面向日志与排障，**不直接回显给
// 终端用户**。Data 用于承载返回值（如新建虚拟机的 UUID）。
type Result struct {
	Success bool
	Message string
	Data    map[string]any
}

// Client 是控制面访问节点的契约。
//
// 每个方法都应被理解为「向节点下一个指令」，而不是本地调用：节点侧需要
// 处理超时、节点离线与重试，这些由实现方承担，调用方只关心结果与错误。
type Client interface {
	// Execute 在指定节点上执行一次领域操作。
	//
	// 返回的错误用于表达「指令未能送达或无法判定结果」（如节点离线），
	// 而操作本身失败应由 Result.Success 为 false 表达——两者语义不同，
	// 调用方需分别处理。
	Execute(ctx context.Context, op Operation) (*Result, error)
}
