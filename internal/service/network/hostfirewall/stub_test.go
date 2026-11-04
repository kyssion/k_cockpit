package hostfirewall_test

import (
	"context"
	"time"

	"k_cockpit/internal/agent"
)

// stubClient 是本包测试用的最小 agent 替身：默认所有操作成功，具体返回值
// 由用例通过 exec 覆盖。它不承担生产行为模拟——生产路径只有真实 gRPC 网关。
type stubClient struct {
	exec func(context.Context, agent.Operation) (*agent.Result, error)
}

func newStubClient() *stubClient { return &stubClient{} }

func (s *stubClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if s.exec != nil {
		return s.exec(ctx, op)
	}
	if op.Kind == agent.OpHostConnections {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.HostConnectionsKey: []agent.HostConnection{
				{RemoteAddr: "203.0.113.9:54321", LocalPort: 22, Protocol: "tcp", State: "ESTABLISHED", Process: "sshd"},
				{RemoteAddr: "198.51.100.4:40404", LocalPort: 8080, Protocol: "tcp", State: "ESTABLISHED", Process: "k_cockpit"},
			},
		}}, nil
	}
	return &agent.Result{Success: true, Message: "stub"}, nil
}

func (s *stubClient) Snapshot(context.Context, int64) (*agent.Snapshot, error) {
	return &agent.Snapshot{Status: agent.StatusOnline, LastHeartbeat: time.Now()}, nil
}

func (s *stubClient) OpenStream(context.Context, agent.StreamKind, int64, string) (agent.Stream, error) {
	return nil, agent.ErrStreamUnsupported
}
