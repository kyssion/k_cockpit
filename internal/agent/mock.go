package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
//
// 对需要返回值的操作给出形状合理的假数据（如创建虚拟机返回 UUID），
// 否则上层拿不到它需要的东西，会在业务代码里被迫写 mock 专用的兜底分支
// ——那正是 ADR-0007 要避免的「业务代码感知 mock」。
func (m *MockClient) Execute(_ context.Context, op Operation) (*Result, error) {
	data := map[string]any{}

	switch op.Kind {
	case OpVMCreate:
		data["uuid"] = mockUUID(op.NodeID, op.Target)
	case OpVMStatus:
		// 固定返回 running（取值与 model.VMStatusRunning 一致；本包不引用
		// model —— 协议层与存储层保持解耦，状态的解释由调用方负责）。
		//
		// 由此产生的局限需要明确：mock 不维护状态，探测结果不随操作变化，
		// 因此**只能验证状态机与拒绝路径**（对 running 的虚拟机执行 start
		// 会被拒绝），**无法验证「先关机再开机」的完整流转**。要打通完整
		// 流转需要 mock 维护一份状态，那正是 ADR-0007 明确不做的事。
		data[StatusDataKey] = "running"
	}

	return &Result{
		Success: true,
		Message: "mock: " + string(op.Kind) + " 已执行",
		Data:    data,
	}, nil
}

// mockUUID 生成稳定且可辨识的假 UUID。
//
// 用运营者能一眼看出是假的格式（前缀 mock-）：如果它长得像真 UUID，
// 排查问题时很容易把它当成真实虚拟化层的标识。
func mockUUID(nodeID int64, name string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", nodeID, name)))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("mock-%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
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
