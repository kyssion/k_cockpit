package publicip_test

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
	case agent.OpPublicIPPreview:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PublicIPPreviewKey: agent.PublicIPPreview{
				Added: []string{"iptables -t nat -A PREROUTING -d 203.0.113.80 -j DNAT --to-destination 10.0.0.5"},
			},
		}}, nil
	case agent.OpPublicIPChange:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PublicIPDataKey: agent.PublicIPInfo{Message: "公网地址规则已更新"},
		}}, nil
	case agent.OpPublicIPReload:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PublicIPReloadDataKey: agent.PublicIPReloadInfo{
				Applied: 1, Message: "已按当前绑定关系重新应用规则",
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
