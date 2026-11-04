package passthrough_test

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
	case agent.OpHostPCIDevices:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PCIDevicesKey: []agent.PCIDevice{
				{Address: "0000:01:00.0", VendorDevice: "10de:1eb8", Class: "VGA",
					Description: "NVIDIA GP104", IOMUGroup: 1,
					GroupMembers: []string{"0000:01:00.1"}, CanPassthrough: true},
				{Address: "0000:01:00.1", VendorDevice: "10de:10f0", Class: "Audio",
					Description: "NVIDIA GP104 Audio", IOMUGroup: 1,
					GroupMembers: []string{"0000:01:00.0"}, CanPassthrough: true},
				{Address: "0000:00:1f.6", VendorDevice: "8086:15b8", Class: "Network",
					Description: "Intel I219-V", IOMUGroup: 2,
					CanPassthrough: false, Reason: "该设备与宿主机管理网络共用，直通会让面板失去连接"},
			},
			agent.IOMMUStatusKey: agent.IOMMUStatus{Enabled: true, VFIOAvailable: true},
		}}, nil
	case agent.OpHostIOMMU:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.IOMMUStatusKey: agent.IOMMUStatus{Enabled: true, VFIOAvailable: true},
		}}, nil
	case agent.OpHostPCIBind:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.HostFirewallDataKey: agent.PCIBindInfo{Message: "设备绑定已更新"},
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
