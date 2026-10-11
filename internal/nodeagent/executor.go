package nodeagent

import (
	"context"
	"fmt"
	"sort"

	"k_cockpit/internal/agent"
)

// Stage 与 agent.Stage 同形（本地别名，执行器不反向依赖 hub 的线上类型）。
type Stage = agent.Stage

// Outcome 是执行器的一次产出；agent.Result 的本地别名。
type Outcome = agent.Result

// Reporter 上报一个新进入的阶段。语义与控制面的任务 Reporter 对接：
// 开始即记录、下一个阶段开始时自动收尾上一个——漏报收尾、重复报都安全。
type Reporter func(Stage)

// Handler 是一个领域操作的本地执行器。
//
// error 只用于「执行器自身崩溃」类异常路径（正常失败返回 Success=false
// 的 Outcome）——与 agent.Client 的 Result/error 二分保持一致。
type Handler func(ctx context.Context, op agent.Operation, report Reporter) (*Outcome, error)

// Registry 是 OpKind → Handler 的执行器注册表。
//
// 注册即能力：Capabilities 直接由它生成，agent 自报的能力清单与实际能
// 执行的操作出自同一处——不会出现「报了不能做」或「能做没报」。
type Registry map[agent.OpKind]Handler

// Capabilities 生成能力清单（排序保证稳定，展示与测试都需要确定性）。
func (r Registry) Capabilities() []string {
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// dispatch 执行一个操作；注册表缺项返回明确的「未实现」结果。
//
// 「未实现」是 Success=false 的正常结果而不是 error：对控制面来说这是一次
// 送达且被拒绝的执行，任务时间线上应当看到它失败的原因，而不是「节点
// 不可达」。
func (r Registry) dispatch(ctx context.Context, op agent.Operation, report Reporter) (out *Outcome) {
	h, ok := r[op.Kind]
	if !ok {
		return &Outcome{
			Success: false,
			Message: fmt.Sprintf("操作 %s 尚未在 agent 侧实现（分期接入中，当前能力见节点能力清单）", op.Kind),
		}
	}
	// 执行器的 panic 在这里拦住：一次操作的崩溃不能带走整个 agent——
	// 带走的表现是所有其它操作全部「与节点的连接已断开」。
	defer func() {
		if rec := recover(); rec != nil {
			out = &Outcome{Success: false, Message: fmt.Sprintf("执行器 panic: %v", rec)}
		}
	}()
	out, err := h(ctx, op, report)
	if err != nil {
		return &Outcome{Success: false, Message: "执行器异常: " + err.Error()}
	}
	if out == nil {
		return &Outcome{Success: false, Message: "执行器未返回结果"}
	}
	return out
}
