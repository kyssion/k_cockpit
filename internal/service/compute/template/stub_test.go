package template_test

import (
	"context"
	"path/filepath"
	"strings"
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
	case agent.OpTemplatePrepare:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.TemplateDataKey: agent.TemplateInfo{
				DiskPath: "/var/lib/k_cockpit/templates/stub.qcow2",
				SizeGB:   20, Format: "qcow2",
			},
		}}, nil
	case agent.OpTemplateImportPreview, agent.OpTemplateImport:
		rel, _ := op.Params["rel_path"].(string)
		base := strings.TrimSuffix(filepath.Base(rel), ".tar.gz")
		name := strings.TrimSuffix(base, ".tgz")
		if name == "" {
			name = "imported-template"
		}
		info := agent.TemplateImportInfo{
			Manifest: agent.TemplateManifest{
				Name: name, Version: 1, DiskFormat: "qcow2", DiskSizeGB: 20,
			},
			Format: "qcow2", SizeGB: 20,
		}
		if strings.Contains(rel, "corrupt") {
			info.DigestMismatch = true
			info.Message = "包内容与清单里的摘要不符"
		} else {
			info.Message = "模板包校验通过"
		}
		if op.Kind == agent.OpTemplateImport {
			info.DiskPath = "/var/lib/k_cockpit/templates/" + name + ".qcow2"
		}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.TemplateImportDataKey: info,
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
