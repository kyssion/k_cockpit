package vm_test

import (
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/compute/vm"
)

// 热迁移收敛阈值的三档判定（F-2-15）：放行 / 限流 / 阻断。
func TestAssessLive(t *testing.T) {
	cases := []struct {
		name          string
		info          agent.MigrateAssessInfo
		wantBlock     bool
		wantConverge  bool
		wantRatioMark bool // DirtyRatioPercent 是否有效（非 -1）
	}{
		{"平稳收敛", agent.MigrateAssessInfo{BandwidthMbps: 937, DirtyRateMBps: 42}, false, false, true},
		{"超过限流阈值", agent.MigrateAssessInfo{BandwidthMbps: 1000, DirtyRateMBps: 110}, false, true, true},
		{"追不上（阻断）", agent.MigrateAssessInfo{BandwidthMbps: 800, DirtyRateMBps: 120}, true, false, true},
		{"无脏页读数", agent.MigrateAssessInfo{BandwidthMbps: 937, DirtyRateMBps: 0}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := vm.AssessLive(tc.info)
			if got.Block != tc.wantBlock {
				t.Errorf("Block = %v, 期望 %v（note=%s）", got.Block, tc.wantBlock, got.Note)
			}
			if got.AutoConverge != tc.wantConverge {
				t.Errorf("AutoConverge = %v, 期望 %v", got.AutoConverge, tc.wantConverge)
			}
			valid := got.DirtyRatioPercent >= 0
			if valid != tc.wantRatioMark {
				t.Errorf("DirtyRatioPercent = %d（有效性 %v, 期望 %v）",
					got.DirtyRatioPercent, valid, tc.wantRatioMark)
			}
			if got.Note == "" {
				t.Error("结论文案不应为空：用户要靠它理解数字意味着什么")
			}
		})
	}
}
