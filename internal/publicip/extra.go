package publicip

import (
	"context"
	"encoding/json"
	"log"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// --- IPv6 前缀检测 ---

// IPv6PrefixView 是一个检测到的 IPv6 前缀。
type IPv6PrefixView struct {
	Prefix     string `json:"prefix"`
	EgressIf   string `json:"egress_if,omitempty"`
	Trusted    bool   `json:"trusted"`
	Assignable int64  `json:"assignable"`
}

// DetectIPv6Prefixes 检测某节点外网网卡上的 IPv6 前缀。
//
// 为什么要"检测"而不是让用户填：一个 /64 前缀有 2^64 个地址，手填既容易错、
// 也无法回答"还剩多少能分"。而"能不能用"取决于本地路由与上游通告，这个判断
// 只有节点能给出（Trusted 字段）。
func (s *Service) DetectIPv6Prefixes(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) ([]IPv6PrefixView, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点，无法检测")
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPublicIPv6Detect,
		NodeID: nodeID,
		Target: "ipv6",
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法检测")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	rows := decodeInto[[]agent.IPv6PrefixInfo](result.Data, agent.IPv6PrefixDataKey)
	out := make([]IPv6PrefixView, 0, len(rows))
	for i := range rows {
		out = append(out, IPv6PrefixView{
			Prefix:     rows[i].Prefix,
			EgressIf:   rows[i].EgressIf,
			Trusted:    rows[i].Trusted,
			Assignable: rows[i].Assignable,
		})
	}
	// 检测出来的**未授信**前缀也要返回：把它们过滤掉会让人以为"检测到的都
	// 能用"，而那正是需要用户自己判断的部分。
	s.record(ctx, auditEntryDetect(v, operatorName, clientIP, nodeID, len(out)))
	return out, nil
}

// --- 重载规则 ---

// ReloadResultView 是重载的结果。
type ReloadResultView struct {
	Applied int    `json:"applied"`
	Failed  int    `json:"failed"`
	Detail  string `json:"detail,omitempty"`
	Message string `json:"message"`
}

// ReloadRules 按当前绑定关系重新应用全部规则。
//
// 它不需要参数：参数是"要改成什么"，而重载做的是"让节点与我们记录的一致"。
// 这对应"记录是对的、节点上漂了"（手工改过、升级残留）这一具体场景，因此
// 它是幂等的——"再点一次"是安全的第一个反应，不必先去查漂移发生在哪一步。
func (s *Service) ReloadRules(
	ctx context.Context, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*ReloadResultView, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点，无法重载")
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPublicIPReload,
		NodeID: nodeID,
		Target: "rules",
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，未重载")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info := decodeInto[agent.PublicIPReloadInfo](result.Data, agent.PublicIPReloadDataKey)
	s.record(ctx, auditEntryReload(v, operatorName, clientIP, nodeID, info.Applied, info.Failed))
	// 有失败的条目也要如实返回：只报"已重载"会让失败的几条被当成成功。
	return &ReloadResultView{
		Applied: info.Applied,
		Failed:  info.Failed,
		Detail:  info.Detail,
		Message: firstNonEmpty(info.Message, "已按当前绑定关系重新应用规则"),
	}, nil
}

// --- 来宾地址状态 ---

// GuestIPView 是一台虚拟机里实际配置的公网地址。
type GuestIPView struct {
	Address    string `json:"address"`
	Family     string `json:"family"`
	Configured bool   `json:"configured"`
	Reachable  bool   `json:"reachable"`
	Detail     string `json:"detail,omitempty"`
}

// GuestStatus 查询某台虚拟机里实际配置的公网地址。
//
// 它回答的是"绑定成功之后为什么还是不通"：**绑定成功 ≠ 来宾里配好了**——
// 控制面只保证规则下发，来宾里还要有人（或 cloud-init）把地址配上。没有这一
// 项，这类问题只能靠进虚拟机自己看。
func (s *Service) GuestStatus(
	ctx context.Context, vmID int64, v authz.Viewer,
) ([]GuestIPView, error) {
	// 这里按 vmID 直接查（没有"地址所在节点"这个约束），权限判断沿用
	// loadVM 的口径：用 404 而不是 403，避免 tenant 通过枚举推断出别人有
	// 多少虚拟机。
	var vm model.VM
	if err := s.db.WithContext(ctx).Where("id = ?", vmID).First(&vm).Error; err != nil {
		return nil, api.NotFound("虚拟机不存在")
	}
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return nil, api.NotFound("虚拟机不存在")
	}
	if s.agent == nil {
		return nil, api.Unavailable("未连接节点，无法查询")
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPublicIPGuestStatus,
		NodeID: vm.NodeID,
		Target: vm.Name,
		Params: map[string]any{"vm_id": vmID},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法查询")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	rows := decodeInto[[]agent.GuestIPInfo](result.Data, agent.GuestIPDataKey)
	out := make([]GuestIPView, 0, len(rows))
	for i := range rows {
		out = append(out, GuestIPView{
			Address: rows[i].Address, Family: rows[i].Family,
			Configured: rows[i].Configured, Reachable: rows[i].Reachable,
			Detail: rows[i].Detail,
		})
	}
	return out, nil
}

// decodeInto 从结果里取一个结构。
//
// 走一遍 JSON：同进程拿到的是结构体、跨进程是 map，两种形状都要能读。
func decodeInto[T any](data map[string]any, key string) T {
	var zero T
	if data == nil {
		return zero
	}
	raw, ok := data[key]
	if !ok {
		return zero
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return zero
	}
	var out T
	if err := json.Unmarshal(blob, &out); err != nil {
		log.Printf("[publicip] 解析 %s 失败: %v", key, err)
		return zero
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// auditEntryDetect 是前缀检测的审计条目。
func auditEntryDetect(v authz.Viewer, operatorName, clientIP string, nodeID int64, found int) audit.Entry {
	return audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "public_ip",
		Action:  "public_ip.ipv6_detect",
		Params:  map[string]any{"found": found},
		Success: true, ClientIP: clientIP,
	}
}

// auditEntryReload 是规则重载的审计条目。
func auditEntryReload(
	v authz.Viewer, operatorName, clientIP string, nodeID int64, applied, failed int,
) audit.Entry {
	return audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "public_ip",
		Action:  "public_ip.reload",
		Params:  map[string]any{"applied": applied, "failed": failed},
		Success: failed == 0, ClientIP: clientIP,
	}
}
