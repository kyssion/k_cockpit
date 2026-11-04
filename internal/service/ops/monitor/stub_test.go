package monitor_test

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
	switch op.Kind {
	case agent.OpHostStats:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.HostStatsDataKey: agent.HostStats{
				CPUPercent: 20, CPUCores: 8, MemUsedMB: 4096, MemTotalMB: 16384,
				Load1: 0.5, Load5: 0.4, Load15: 0.3,
				NetInBytes: 1 << 20, NetOutBytes: 2 << 20,
				DiskReadBytes: 3 << 20, DiskWriteBytes: 4 << 20, UptimeSeconds: 3600,
			},
		}}, nil
	case agent.OpVMStats:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.StatsDataKey: agent.VMStats{
				CPUPercent: 12, MemTotalMB: 2048, MemUsedMB: 1024,
				NetRxKbps: 100, NetTxKbps: 50, DiskReadKbps: 20, DiskWriteKbps: 10,
				UptimeSeconds: 600,
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
