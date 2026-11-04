package bridge_test

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
	case agent.OpNetworkProbe:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.NetworkCapabilityKey: agent.NetworkCapability{
				OVSAvailable:  false,
				KernelModules: []string{"bridge"},
				UplinkCandidates: []agent.UplinkCandidate{
					{Name: "eth0", Up: true, HasIP: true, Speed: "1000Mb/s"},
					{Name: "eth1", Up: true, HasIP: false, Speed: "1000Mb/s"},
				},
				Notes: []string{"OVS 未安装，已降级到 Linux 网桥"},
			},
		}}, nil
	case agent.OpNetworkRepair:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.NetworkRepairKey: agent.NetworkRepairInfo{
				Fixed:     []string{"br-ovs 已置 UP"},
				Remaining: []string{"OVS 未安装，无法启用增强网络"},
				Message:   "已按现状收敛",
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
