package vm

import (
	"context"
	"fmt"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// 迁移方式。
const (
	// MigrationOffline 停机迁移：先把机器关掉，复制完磁盘之后在目标节点上
	// 启动。
	MigrationOffline = "offline"
	// MigrationLive 热迁移（F-2-15）：运行中的机器不停机搬到目标节点，
	// 只付出一个通常几百毫秒的停顿窗口。能否收敛取决于脏页速率与链路
	// 带宽的比值，预检会给出量化结论。
	MigrationLive = "live"
)

// MigrationPreview 是迁移前的预检结果。
//
// 它要回答的是用户点下按钮**之前**唯一想知道的那件事：
//
//	**这次要停多久？**
//
// 因此除了"能不能迁"（Ready / Blockers），还必须给出**会怎么迁**
// （Mode / DataVolume）——因为停机时长由这两件事决定，而不是由"能不能迁"
// 决定。热迁移落地后 Mode 由机器的实时状态决定：运行中走 live（停顿窗口
// 毫秒级），已关机走 offline（停机时长≈复制时长）。
type MigrationPreview struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`

	FromNodeID   int64  `json:"from_node_id"`
	FromNodeName string `json:"from_node_name"`
	ToNodeID     int64  `json:"to_node_id"`
	ToNodeName   string `json:"to_node_name"`

	// Ready 为 false 时 Blockers 说明原因。
	//
	// **它是"现在能不能迁"，而不是"这台机器值不值得迁"**——后者不是接口
	// 能回答的。
	Ready bool `json:"ready"`
	// Blockers 是会**阻止**迁移的条件，与 Migrate 用的是同一套校验。
	Blockers []string `json:"blockers,omitempty"`
	// Warnings 不阻止迁移但用户应当知道的事。
	Warnings []string `json:"warnings,omitempty"`

	// Mode 是迁移会走的方式（live / offline）。
	Mode string `json:"mode"`
	// ModeNote 用人话解释这种方式意味着什么。
	ModeNote string `json:"mode_note"`

	// DiskGB 是要复制的数据量。
	//
	// 它是停机时长的**主要因素**，而用户通常不记得自己那台机器有多大。
	DiskGB int `json:"disk_gb"`
	// DowntimeHint 给出停机时长的**量级与依据**，而不是一个精确数字。
	//
	// 精确数字依赖于目标节点之间的实际带宽，而那只有真正开始传才知道。
	// 给一个精确到秒的估计会让用户按它去安排窗口期——然后发现差了几倍。
	// 相比之下「取决于 X，通常在这个量级」不会误导人。
	DowntimeHint string `json:"downtime_hint"`
	// BandwidthMbps 是两节点间**实测**的可用带宽（G-35）；测不到时为 0，
	// 此时 DowntimeHint 回落到按链路规格的口径。
	BandwidthMbps int64 `json:"bandwidth_mbps,omitempty"`
	// BandwidthSource 说明带宽的来源（speedtest / estimate），两者置信度
	// 不同，界面上要能区分。
	BandwidthSource string `json:"bandwidth_source,omitempty"`

	// --- 热迁移的量化评估（F-2-15，仅 Mode = live 时有值）---

	// DirtyRateMBps 是评估时测得的脏页速率；0 表示未测得。
	DirtyRateMBps int64 `json:"dirty_rate_mbps,omitempty"`
	// DirtyRatioPercent 是脏页带宽占链路带宽的百分比；-1 表示没有读数。
	DirtyRatioPercent int `json:"dirty_ratio_percent,omitempty"`
	// AutoConverge 表示执行时会自动开启 CPU 限流（比值超过 80%）。
	AutoConverge bool `json:"auto_converge,omitempty"`
	// ConvergeNote 是收敛性的结论（带数字），预览页直接展示。
	ConvergeNote string `json:"converge_note,omitempty"`
}

// PreviewMigration 预检一次迁移。**只读**，不产生任何记录。
//
// **它与 Migrate 共用同一套校验**（见 migrationBlockers）。分两处写的话
// 迟早会分叉，而分叉的表现是最难解释的一种：**预览说可以，点下去被拒**。
// 用户会反复确认自己的操作，而问题在于两处用了不同的规则。
func (s *Service) PreviewMigration(
	ctx context.Context, id, toNodeID int64, v authz.Viewer,
) (*MigrationPreview, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if toNodeID <= 0 {
		return nil, api.InvalidParameter("必须指定目标节点")
	}

	blockers, mode := s.migrationBlockers(ctx, target, toNodeID)

	out := &MigrationPreview{
		VMID: target.ID, VMName: target.Name,
		FromNodeID: target.NodeID,
		ToNodeID:   toNodeID,
		Mode:       mode,
		DiskGB:     target.DiskGB,
	}
	out.FromNodeName = s.nodeName(ctx, target.NodeID)
	out.ToNodeName = s.nodeName(ctx, toNodeID)

	// **与 Migrate 同一套校验。**
	out.Blockers = errStrings(blockers)
	out.Ready = len(out.Blockers) == 0

	switch mode {
	case MigrationLive:
		// 迁移方式必须说明白：热迁移不是"零停机"，而是"停顿窗口毫秒级"。
		// 说零停机的人会在切换那一刻发现服务断了一下，然后不再信任面板。
		out.ModeNote = "运行中的虚拟机**不停机**搬到目标节点，只在切换那一刻" +
			"有一个通常几百毫秒的停顿窗口。"
		if out.Ready {
			s.previewLive(ctx, target, toNodeID, out)
		}
	default:
		out.Mode = MigrationOffline
		out.ModeNote = "先把虚拟机**关机**，把磁盘完整复制到目标节点，再在那边启动。" +
			"因此停机时长约等于复制整个磁盘所需的时间。"
		if target.DiskGB > 0 {
			out.DowntimeHint = s.migrationDowntimeHint(ctx, target, toNodeID, out)
		}
	}
	if target.DiskGB == 0 {
		out.Warnings = append(out.Warnings, "这台虚拟机没有记录磁盘容量，停机时长无法估算")
	}
	// 目标节点上的资源量是一个值得提前知道的事——迁移之后所有 IO 都在那边，
	// 而用户在源节点上已经习惯了当前的性能。
	out.Warnings = append(out.Warnings,
		"迁移会改变虚拟机所在的宿主机：IP 与端口转发会跟着走，"+
			"但存储与 CPU 的实际性能取决于目标节点。")

	return out, nil
}

// previewLive 填充热迁移的量化评估（F-2-15）。
//
// 评估不出来**不阻断**预检：它只是收敛性的依据，测不到时如实说"无法
// 预判"，迁移仍可进行——节点侧有自己的超时与失败上报，那才是真正的
// 执行边界。
func (s *Service) previewLive(
	ctx context.Context, target *model.VM, toNodeID int64, out *MigrationPreview,
) {
	info, ok := s.assessLine(ctx, target, toNodeID)
	if ok {
		out.BandwidthMbps = info.BandwidthMbps
		out.BandwidthSource = info.Source
	}
	live := AssessLive(info)
	out.DirtyRateMBps = live.DirtyRateMBps
	out.DirtyRatioPercent = live.DirtyRatioPercent
	out.AutoConverge = live.AutoConverge
	out.ConvergeNote = live.Note
	if live.Block {
		// 唯一升级成 blocker 的评估结论：放行一个追不上的热迁移，结果
		// 一定是跑满时长然后超时——机器在源侧白白卡了那么久。
		out.Blockers = append(out.Blockers, live.Note)
		out.Ready = false
	}
}

// assessLine 向源节点发起一次线路评估（带宽 + 脏页速率）。
func (s *Service) assessLine(
	ctx context.Context, target *model.VM, toNodeID int64,
) (agent.MigrateAssessInfo, bool) {
	if s.agent == nil {
		return agent.MigrateAssessInfo{}, false
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpMigrateAssess,
		NodeID: target.NodeID,
		Target: target.Name,
		Params: map[string]any{"target_node_id": toNodeID, "live": true},
	})
	if err != nil || !result.Success {
		return agent.MigrateAssessInfo{}, false
	}
	info, ok := result.Data[agent.MigrateAssessDataKey].(agent.MigrateAssessInfo)
	return info, ok
}

// migrationDowntimeHint 生成停机时长的估算文案（G-35）。
//
// 优先使用**实测带宽**（节点间真实传输一小段数据计时），拿不到时回落到
// 按千兆链路的口径。两条路径的差别是置信度：实测值能把「大约 7 分钟」
// 收窄到「大约 8 分钟」，而回落路径连数量级都可能差十倍——界面上必须
// 能看出当前给的是哪一种。
func (s *Service) migrationDowntimeHint(
	ctx context.Context, target *model.VM, toNodeID int64, out *MigrationPreview,
) string {
	// 测速失败不阻断预检：带宽只是一个估算依据，评估不出来说明原因，
	// 然后回落到公式——迁移本身在其余校验通过时仍然可以进行。
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpMigrateAssess,
		NodeID: target.NodeID,
		Target: target.Name,
		Params: map[string]any{"target_node_id": toNodeID},
	})
	if err == nil && result.Success {
		if info, ok := result.Data[agent.MigrateAssessDataKey].(agent.MigrateAssessInfo); ok && info.BandwidthMbps > 0 {
			out.BandwidthMbps = info.BandwidthMbps
			out.BandwidthSource = info.Source
			// MB/s = Mbps / 8；分钟 = GB * 1024 MB / (MB/s) / 60。
			minutes := int64(target.DiskGB) * 1024 / (info.BandwidthMbps / 8) / 60
			hint := fmt.Sprintf(
				"实测两节点间带宽约 %d Mbps%s。%d GB 的数据预计需要 %d 分钟；"+
					"实际时长取决于传输时的并发负载，迁移期间虚拟机保持关机。",
				info.BandwidthMbps, sourceNote(info.Source), target.DiskGB, minutes)
			if info.Message != "" {
				hint += "（" + info.Message + "）"
			}
			return hint
		}
	}
	out.Warnings = append(out.Warnings,
		"未能测得两节点间的实际带宽，以下估算按千兆链路的常见值计算")
	return fmt.Sprintf(
		"取决于两个节点之间的实际带宽。以千兆网（约 100 MB/s）为例，"+
			"%d GB 的数据大约需要 %d 分钟；万兆网快约十倍。"+
			"迁移期间虚拟机保持关机。",
		target.DiskGB, target.DiskGB*1024/100/60)
}

// sourceNote 把来源标识翻成人话，说明这个数字有多可信。
func sourceNote(source string) string {
	if source == agent.AssessSourceEstimate {
		return "（按链路规格估算）"
	}
	return ""
}

// migrationBlockers 返回会阻止迁移的条件，以及探测到的状态与由此决定的
// 迁移方式。
//
// **Migrate 与 PreviewMigration 都调它**——这是"预览说可以、点下去被拒"
// 这类问题的唯一防线。
//
// 返回**原始错误而不是字符串**：状态码本身携带信息（409 是冲突、404 是
// 不存在、422 是参数或前置条件不满足），而客户端据此决定怎么呈现。压成
// 字符串会让 Migrate 只能把它们统一成 422——那是个真实的回归，测试直接
// 抓到了它。
//
// 状态与方式的对应（F-2-15）：运行中 → 热迁移；已关机 → 停机迁移；
// 暂停 / 挂起 / 错误 / 未知 → 阻断（热迁移要求源在跑，停机迁移要求源
// 已停，两头都不沾的状态只能让用户先把它带到其中一边）。
func (s *Service) migrationBlockers(
	ctx context.Context, target *model.VM, toNodeID int64,
) ([]error, string) {
	out := []error{}
	mode := MigrationOffline

	if toNodeID == target.NodeID {
		return append(out, api.ValidationFailed("目标节点与当前节点相同，无需迁移")), mode
	}
	if err := s.ensureMigrateTarget(ctx, toNodeID); err != nil {
		out = append(out, err)
	}

	// 实时探测，不用投影（f-2-01 R-002）。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		out = append(out, api.Unavailable("无法确认虚拟机的当前状态（节点不可达）"))
	} else {
		switch current {
		case model.VMStatusRunning:
			mode = MigrationLive
		case model.VMStatusStopped:
			// 停机迁移。
		default:
			out = append(out, api.ValidationFailed(
				"当前状态（"+DescribeStatus(current)+"）两种迁移方式都不适用："+
					"热迁移需要机器在运行，停机迁移需要机器已关机。"+
					"请先恢复或关机后再迁移"))
		}
	}

	active, err := s.hasActiveTask(ctx, target.ID)
	if err != nil {
		out = append(out, api.Internal())
	} else if active {
		out = append(out, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消"))
	}

	// 节点内资源的冲突检查。这些是迁移**独有**的一类前置条件：静态地址与
	// 端口转发在各自节点内唯一，换一台宿主机就可能撞上别人。
	conflicts, err := s.migrationConflicts(ctx, target, toNodeID)
	if err != nil {
		out = append(out, api.Internal())
	} else if len(conflicts) > 0 {
		out = append(out, api.Conflict("目标节点上存在冲突："+joinConflicts(conflicts)+
			"。这些资源在节点内唯一，请先在目标节点上释放它们，或先在源节点上解绑"))
	}
	return out, mode
}

// errStrings 把错误列表转成文案，供预览展示。
func errStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, errMessage(e))
	}
	return out
}

func (s *Service) nodeName(ctx context.Context, nodeID int64) string {
	var node model.Node
	if err := s.db.WithContext(ctx).Select("name").First(&node, nodeID).Error; err != nil {
		return fmt.Sprintf("#%d", nodeID)
	}
	return node.Name
}
