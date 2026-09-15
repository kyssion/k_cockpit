package agent

import (
	"context"
	"time"
)

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

// Snapshot 返回固定的运行态：一律「在线」，心跳时间取当前时刻。
//
// 心跳时间用 `time.Now()` 而不是固定值，是为了让界面上的「最后心跳」
// 不至于随着服务运行时间推移显示成几小时前——那看起来像故障。
//
// 注意：这意味着**离线分支在接入真实 agent 前不会被触发**，
// 包括「离线标记」与「离线时不派发任务」等逻辑，需在联调时集中验证
// （见 docs/06-decisions/0007-mock-agent-first.md）。
func (m *MockClient) Snapshot(_ context.Context, _ int64) (*Snapshot, error) {
	now := time.Now()
	return &Snapshot{
		Status:          StatusOnline,
		LastHeartbeat:   now,
		AgentVersion:    "mock-0.1.0",
		ProtocolVersion: 1,
		Capabilities: []string{
			"vm.create", "vm.start", "vm.stop", "vm.delete", "vm.snapshot",
			"storage.pool.create", "network.bridge.list", "network.ovs.configure",
		},
		CapabilitiesAt: now,
	}, nil
}

// 编译期断言：MockClient 必须满足 Client 契约。
var _ Client = (*MockClient)(nil)
