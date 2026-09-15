package agent

import "context"

// MockClient 是 Client 的假实现。
//
// 它**只保证接口有返回值**，不模拟节点行为：不模拟耗时、进度推进、失败注入
// 与离线（见 docs/06-decisions/0007-mock-agent-first.md）。接入真实 agent 前，
// 执行失败、超时、节点离线等分支不会被触发，需在联调时集中验证。
type MockClient struct{}

// NewMockClient 构造假实现。
func NewMockClient() *MockClient { return &MockClient{} }

// Execute 直接返回成功，不产生任何副作用。
func (m *MockClient) Execute(_ context.Context, op Operation) (*Result, error) {
	return &Result{
		Success: true,
		Message: "mock: " + string(op.Kind) + " 已执行",
		Data:    map[string]any{},
	}, nil
}

// 编译期断言：MockClient 必须满足 Client 契约。
var _ Client = (*MockClient)(nil)
