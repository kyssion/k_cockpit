package vm

import (
	"context"
	"fmt"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// 迁移方式。
const (
	// MigrationOffline 停机迁移：先把机器关掉，复制完磁盘之后在目标节点上
	// 启动。**这是当前唯一支持的方式。**
	MigrationOffline = "offline"
)

// MigrationPreview 是迁移前的预检结果。
//
// 它要回答的是用户点下按钮**之前**唯一想知道的那件事：
//
//	**这次要停多久？**
//
// 因此除了"能不能迁"（Ready / Blockers），还必须给出**会怎么迁**
// （Mode / DataVolume）——因为停机时长由这两件事决定，而不是由"能不能迁"
// 决定。
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

	// Mode 是迁移会走的方式。
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

	out := &MigrationPreview{
		VMID: target.ID, VMName: target.Name,
		FromNodeID: target.NodeID,
		ToNodeID:   toNodeID,
		Mode:       MigrationOffline,
		DiskGB:     target.DiskGB,
	}
	out.FromNodeName = s.nodeName(ctx, target.NodeID)
	out.ToNodeName = s.nodeName(ctx, toNodeID)

	// **与 Migrate 同一套校验。**
	out.Blockers = errStrings(s.migrationBlockers(ctx, target, toNodeID))
	out.Ready = len(out.Blockers) == 0

	// 迁移方式：当前只有停机迁移，而这件事必须说明白——它决定了停机时长，
	// 而用户很可能以为迁移是"不断服务地挪过去"。
	out.ModeNote = "先把虚拟机**关机**，把磁盘完整复制到目标节点，再在那边启动。" +
		"因此停机时长约等于复制整个磁盘所需的时间。"
	if target.DiskGB > 0 {
		// 见 DowntimeHint 的说明：给量级与依据，不给精确数字。
		out.DowntimeHint = fmt.Sprintf(
			"取决于两个节点之间的实际带宽。以千兆网（约 100 MB/s）为例，"+
				"%d GB 的数据大约需要 %d 分钟；万兆网快约十倍。"+
				"迁移期间虚拟机保持关机。",
			target.DiskGB, target.DiskGB*1024/100/60)
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

// migrationBlockers 返回会阻止迁移的条件。
//
// **Migrate 与 PreviewMigration 都调它**——这是"预览说可以、点下去被拒"
// 这类问题的唯一防线。
//
// 返回**原始错误而不是字符串**：状态码本身携带信息（409 是冲突、404 是
// 不存在、422 是参数或前置条件不满足），而客户端据此决定怎么呈现。压成
// 字符串会让 Migrate 只能把它们统一成 422——那是个真实的回归，测试直接
// 抓到了它。
func (s *Service) migrationBlockers(
	ctx context.Context, target *model.VM, toNodeID int64,
) []error {
	out := []error{}

	if toNodeID == target.NodeID {
		return append(out, api.ValidationFailed("目标节点与当前节点相同，无需迁移"))
	}
	if err := s.ensureMigrateTarget(ctx, toNodeID); err != nil {
		out = append(out, err)
	}

	// 实时探测，不用投影（f-2-01 R-002）。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		out = append(out, api.Unavailable("无法确认虚拟机的当前状态（节点不可达）"))
	} else if current != model.VMStatusStopped {
		out = append(out, api.ValidationFailed(
			"迁移需要先关机（当前："+DescribeStatus(current)+"）。"+
				"运行中迁移会让磁盘在被写入的同时被复制，两侧都不可用"))
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
	return out
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
