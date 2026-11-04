package portsecurity_test

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
	case agent.OpPortSecurityPrecheck:
		pps := 0
		switch v := op.Params["pps_limit"].(type) {
		case int:
			pps = v
		case int64:
			pps = int(v)
		case float64:
			pps = int(v)
		}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PortSecurityPrecheckKey: agent.PortSecurityPrecheck{
				Capabilities: []agent.PortSecurityCapability{
					{Key: "ovs", Label: "OVS 流表", Required: true},
					{Key: "ofctl", Label: "OpenFlow 工具", Required: true},
					{Key: "meter", Label: "OVS meter（包速率限制）", Required: pps > 0,
						Missing: true, Reason: "当前 OVS 不支持 meter 表", Fix: "升级到支持 meter 的 OVS 版本"},
				},
				Rules:    []string{"priority=100 in_port=1 actions=drop"},
				Warnings: []string{"该网口已有手工流表，启用后会被覆盖"},
			},
		}}, nil
	case agent.OpPortSecurityApply:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PortSecurityDataKey: agent.PortSecurityInfo{Applied: true, Message: "端口安全已应用"},
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
