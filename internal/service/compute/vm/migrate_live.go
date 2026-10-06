package vm

import (
	"fmt"

	"k_cockpit/internal/agent"
)

// 热迁移的收敛阈值（F-2-15）：脏页速率折算成带宽后与实测带宽的比值。
//
//   - ≥ liveBlockRatio：永远追不上，直接阻断——放行的话迁移会跑满时长
//     然后超时失败，机器在源侧卡了半天什么都没换来；
//   - ≥ liveConvergeRatio：勉强能收敛但会很慢，自动开启 CPU 限流
//     （auto-converge）压住脏页产生速度——"限流或阻断"里的前一半。
const (
	liveBlockRatio    = 1.0
	liveConvergeRatio = 0.8
)

// liveAssessment 是一次热迁移的量化评估结论。
type liveAssessment struct {
	DirtyRateMBps int64
	BandwidthMbps int64
	// DirtyRatioPercent 是脏页带宽占链路带宽的百分比（0-…），-1 表示没有
	// 脏页读数（评估不出来）。
	DirtyRatioPercent int
	// AutoConverge 为 true 表示比值超过限流阈值，执行时应开启 CPU 限流。
	AutoConverge bool
	// Block 为 true 表示比值超过阻断阈值。
	Block bool
	// Note 是给人看的结论（带数字），进预览的 ModeNote / DowntimeHint。
	Note string
}

// AssessLive 按"脏页速率 ÷ 链路带宽"的比值给出热迁移结论（F-2-15）。
//
// 导出供测试钉住阈值规则：mock 的评估读数固定，端到端构造不出"追不上"
// 的场景，而阈值规则（阻断 / 限流 / 放行）恰恰是本功能最需要钉住的部分。
func AssessLive(info agent.MigrateAssessInfo) liveAssessment {
	out := liveAssessment{
		DirtyRateMBps: info.DirtyRateMBps,
		BandwidthMbps: info.BandwidthMbps,
	}
	if info.DirtyRateMBps <= 0 || info.BandwidthMbps <= 0 {
		out.DirtyRatioPercent = -1
		out.Note = "未能测得脏页速率，热迁移的收敛性无法预判；如执行，节点会在"
		out.Note += "迁移超时时如实失败"
		return out
	}
	// 脏页 MB/s × 8 折算成 Mbps 再与带宽比。
	ratio := float64(info.DirtyRateMBps*8) / float64(info.BandwidthMbps)
	out.DirtyRatioPercent = int(ratio * 100)

	switch {
	case ratio >= liveBlockRatio:
		out.Block = true
		out.Note = fmt.Sprintf(
			"脏页速率 %d MB/s 折算 %d Mbps，已达链路带宽（%d Mbps）的 %d%%："+
				"内存写入比传输快，热迁移无法收敛。请先降低负载（或关掉写入密集的进程）再迁移，"+
				"或改走停机迁移",
			info.DirtyRateMBps, info.DirtyRateMBps*8, info.BandwidthMbps, out.DirtyRatioPercent)
	case ratio >= liveConvergeRatio:
		out.AutoConverge = true
		out.Note = fmt.Sprintf(
			"脏页速率 %d MB/s 折算 %d Mbps，占链路带宽（%d Mbps）的 %d%%："+
				"收敛会很慢，执行时将自动开启 CPU 限流（auto-converge）压住脏页产生速度",
			info.DirtyRateMBps, info.DirtyRateMBps*8, info.BandwidthMbps, out.DirtyRatioPercent)
	default:
		out.Note = fmt.Sprintf(
			"脏页速率 %d MB/s 折算 %d Mbps，占链路带宽（%d Mbps）的 %d%%，"+
				"预计可以平稳收敛",
			info.DirtyRateMBps, info.DirtyRateMBps*8, info.BandwidthMbps, out.DirtyRatioPercent)
	}
	return out
}

// observedStatusOf 由迁移方式反推受理时探测到的状态：
// 热迁移只对运行中的机器受理，停机迁移只对已关机的机器受理。
func observedStatusOf(mode string) string {
	if mode == MigrationLive {
		return "running"
	}
	return "stopped"
}
