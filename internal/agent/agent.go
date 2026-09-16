// Package agent 定义控制面与节点 agent 的交互契约。
//
// 控制面不直连宿主机（[ADR-0005]）：全部宿主侧动作都经本包下达。
//
// 本期**不开发 agent 侧**：接口只保留契约，由 mock 直接返回结果（[ADR-0007]）。
// 业务代码只依赖 Client，不知道背后是 mock 还是真实的 gRPC 实现，因此接入
// 真实节点时只需替换实现，调用方无需改动。
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
)

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
// 每个方法都应被理解为「向节点下一个指令」，而不是本地调用：真实实现需要
// 处理超时、节点离线与重试，这些由实现方承担，调用方只关心结果与错误。
type Client interface {
	// Execute 在指定节点上执行一次领域操作。
	//
	// 返回的错误用于表达「指令未能送达或无法判定结果」（如节点离线），
	// 而操作本身失败应由 Result.Success 为 false 表达——两者语义不同，
	// 调用方需分别处理。
	Execute(ctx context.Context, op Operation) (*Result, error)
}
