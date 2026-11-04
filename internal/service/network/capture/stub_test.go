package capture_test

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
	case agent.OpNetworkCapture:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.CaptureDataKey: agent.CaptureInfo{
				FilePath:  "/var/lib/k_cockpit/captures/stub.pcap",
				SizeBytes: 0,
				Message:   "抓包进行中，约 10 秒后可取回",
			},
		}}, nil
	case agent.OpCaptureFetch:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.CaptureContentKey: agent.ExportContent{
				Data: []byte("stub-pcap"), MIME: "application/vnd.tcpdump.pcap",
			},
		}}, nil
	case agent.OpNetworkCaptureDelete:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.CaptureDataKey: agent.CaptureInfo{Message: "抓包文件已删除"},
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
