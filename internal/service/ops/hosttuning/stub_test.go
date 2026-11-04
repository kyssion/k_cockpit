package hosttuning_test

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
	if op.Kind == agent.OpHostTuning {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.TuningStateKey: agent.TuningState{
				KSM: agent.KSMState{
					Enabled: true, PagesShared: 42, PagesSharing: 55,
					PagesUnshared: 1200000, SavedBytes: 53 * 4096, FullScans: 1841, RunMode: "always",
				},
				ZRAM: agent.ZRAMState{
					Enabled: true, DisksizeBytes: 4 << 30, UsedBytes: 512 << 20,
					OrigDataBytes: 2 << 30, Algorithm: "lz4", MemLimitBytes: 2 << 30,
				},
				Nested: agent.NestedState{
					Enabled: true, Supported: true, Persistent: false,
					Fix: "写入 /etc/modprobe.d/kvm-nested.conf 并重建 initramfs 后重启",
				},
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
