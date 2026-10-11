// helpers.go：执行器共用的小工具。
package nodeagent

import "k_cockpit/internal/agent"

// failResult 构造一次「送达但失败」的结果（Result 语义，不是 error）。
func failResult(msg string) *agent.Result {
	return &agent.Result{Success: false, Message: msg}
}
