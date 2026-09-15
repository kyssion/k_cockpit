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
	OpVMCreate OpKind = "vm.create"
	OpVMStart  OpKind = "vm.start"
	OpVMStop   OpKind = "vm.stop"
	OpVMDelete OpKind = "vm.delete"
)

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
