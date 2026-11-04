package node_test

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
	if op.Kind == agent.OpNodeStats {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.NodeStatsDataKey: agent.NodeStats{
				CPUPercent: 12.5, CPUCores: 8, LoadAvg1: 0.4, LoadAvg5: 0.3, LoadAvg15: 0.2,
				MemTotalMB: 16384, MemUsedMB: 4096,
				DiskTotalBytes: 1 << 40, DiskUsedBytes: 1 << 38, UptimeSeconds: 3600,
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
