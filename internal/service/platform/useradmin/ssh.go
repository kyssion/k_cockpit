package useradmin

import (
	"context"
	"encoding/json"
	"log"
	"strconv"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
)

// SSHAccessResult 是设置 SSH 访问的结果。
type SSHAccessResult struct {
	User View `json:"user"`
	// KilledSessions 是被结束的在线会话数。
	//
	// 回显它，是为了让界面能说清"改了配置，并且把已经进去的人请出去了"——
	// 否则用户无从判断这次操作是否真的生效：只改配置而人还在里面，等于把
	// "禁止访问"变成了一句只对下次登录生效的话。
	KilledSessions int    `json:"killed_sessions"`
	Message        string `json:"message,omitempty"`
	// Unavailable 非空表示节点没实现（控制面记录已改，但宿主机上未生效）。
	Unavailable string `json:"unavailable,omitempty"`
}

// SetSSHAccess 启用或禁用某用户的 SSH 访问（F-1-10）。
//
// 控制面**不持有到宿主机的登录通道**（迁移 0002 已删掉 ssh_host / ssh_user
// 那些列），因此这里只下发 `user.ssh.access`，由节点改登录 shell、sshd 拒绝
// 名单并结束在线会话。
//
// 即使节点没实现，控制面记录也要改：列表上显示的"允许 / 不允许"是**我们的
// 意图**，而宿主机上是否生效由 Unavailable 单独说明。这两件事混在一起会让
// "明明禁用了却还能登"变成无法解释的问题。
// enrolledNodeIDs 返回全部已接入节点（SSH 开关的目标集合）。
func (s *Service) enrolledNodeIDs(ctx context.Context) []int64 {
	var ids []int64
	if err := s.db.WithContext(ctx).Model(&model.Node{}).
		Where("enroll_state = ? AND deleted_at IS NULL", model.NodeEnrollEnrolled).
		Order("id").Pluck("id", &ids).Error; err != nil {
		return nil
	}
	return ids
}

func (s *Service) SetSSHAccess(
	ctx context.Context, id int64, enabled bool, v authz.Viewer, operatorName, clientIP string,
) (*SSHAccessResult, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	// 不允许改自己：把自己关在门外是一次无法自助恢复的操作（没人能再进管理
	// 界面把它开回来）。
	if v.UserID == row.ID {
		return nil, api.ValidationFailed("不能修改自己的 SSH 访问权限")
	}

	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{"ssh_access_enabled": enabled}).Error; err != nil {
		log.Printf("[useradmin] 更新 SSH 权限失败: %v", err)
		return nil, api.Internal()
	}

	// 开启 SSH 访问与封禁账号是相反方向的动作，状态上必须一致：一个被封禁的
	// 用户不该同时被允许 SSH 登录宿主机。
	if enabled && row.Status == model.UserStatusBanned {
		if err := s.db.WithContext(ctx).Model(&model.User{}).
			Where("id = ?", row.ID).
			Updates(map[string]any{"ssh_access_enabled": false}).Error; err == nil {
			row.SSHAccessEnabled = false
			return nil, api.ValidationFailed("该用户处于封禁状态，不能开启 SSH 访问")
		}
	}

	out := &SSHAccessResult{}
	// 目标节点：SSH 的落点是宿主机，但真实通道里 NodeID 是路由键（0 永远
	// 不在线——mock 不看 NodeID 的时代遗留，与 passaudit 同型；review
	// 抓出）。单节点部署取第一个已接入节点；多节点时遍历全部，任一失败
	// 如实带出。
	nodeIDs := s.enrolledNodeIDs(ctx)
	if s.agent == nil || len(nodeIDs) == 0 {
		out.Unavailable = "没有已接入的节点，宿主机上尚未生效"
	} else {
		// unreachable 计数独立于业务结果：一台不可达不能把已生效的节点
		// 遮蔽成「整批不可达」（此前 lastErr 只在部分路径被清，多节点时
		// 误报且丢失成功节点的结果——review 抓出）。
		unreachable := 0
		var lastResult *agent.Result
		for _, nid := range nodeIDs {
			result, err := s.agent.Execute(ctx, agent.Operation{
				Kind:   agent.OpUserSSHAccess,
				NodeID: nid,
				Target: row.Username,
				Params: map[string]any{"username": row.Username, "enabled": enabled},
			})
			if err != nil {
				unreachable++
				continue
			}
			lastResult = result
			if !result.Success {
				break
			}
		}
		switch {
		case lastResult != nil && !lastResult.Success:
			out.Unavailable = lastResult.Message
		case lastResult == nil:
			out.Unavailable = "节点不可达，宿主机上尚未生效"
		default:
			info := decodeSSHAccess(lastResult.Data)
			out.KilledSessions = info.KilledSessions
			out.Message = info.Message
			if unreachable > 0 {
				out.Message += "；另有 " + strconv.Itoa(unreachable) +
					" 台节点不可达，宿主机上未生效"
			}
		}
	}

	row.SSHAccessEnabled = enabled
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		ResourceType: "user", ResourceID: row.ID, ResourceName: row.Username,
		Action: "user.ssh_access",
		Params: map[string]any{
			"enabled": enabled,
			"killed":  strconv.Itoa(out.KilledSessions),
		},
		Success: out.Unavailable == "", ClientIP: clientIP,
	})

	view := toView(row, nil)
	out.User = view
	return out, nil
}

func decodeSSHAccess(data map[string]any) agent.UserSSHAccessInfo {
	if data == nil {
		return agent.UserSSHAccessInfo{}
	}
	raw, ok := data[agent.UserSSHAccessDataKey]
	if !ok {
		return agent.UserSSHAccessInfo{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.UserSSHAccessInfo{}
	}
	var info agent.UserSSHAccessInfo
	if err := json.Unmarshal(blob, &info); err != nil {
		log.Printf("[useradmin] 解析 SSH 设置结果失败: %v", err)
		return agent.UserSSHAccessInfo{}
	}
	return info
}
