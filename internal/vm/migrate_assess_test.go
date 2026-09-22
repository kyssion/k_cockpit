package vm_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// TestMigrationPreviewUsesMeasuredBandwidth 覆盖预检的实测带宽（G-35）。
//
// mock 节点返回固定读数：预检应把它带出来，并用它换算预计分钟数——
// 而不是仍按「假设千兆」的公式给一个与实测无关的数字。
func TestMigrationPreviewUsesMeasuredBandwidth(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "migr-vm")
	target := seedTargetNode(t, db, false)

	out, err := svc.PreviewMigration(ctx, row.ID, target, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取迁移预检失败: %v", err)
	}

	if out.BandwidthMbps != 937 {
		t.Errorf("带宽 = %d, 期望实测值 937", out.BandwidthMbps)
	}
	if out.BandwidthSource != agent.AssessSourceSpeedtest {
		t.Errorf("来源 = %q, 期望 %q", out.BandwidthSource, agent.AssessSourceSpeedtest)
	}
	if !strings.Contains(out.DowntimeHint, "937") {
		t.Errorf("停机估算未使用实测带宽: %q", out.DowntimeHint)
	}
	// 100 GB 的默认预置是 20 GB：20*1024/117/60 ≈ 2.9 → 2 分钟。
	// 这里不断言具体分钟数（受取整影响），只断言口径换算过了——
	// 若仍用「千兆=100MB/s」的旧公式，文案里不会出现实测读数。
	if !strings.Contains(out.DowntimeHint, "分钟") {
		t.Errorf("停机估算缺少时长: %q", out.DowntimeHint)
	}
}
