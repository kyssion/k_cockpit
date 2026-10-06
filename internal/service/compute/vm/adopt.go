package vm

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
)

// UnmanagedDomain 是节点上发现、但面板还没有记录的域。
type UnmanagedDomain struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	Autostart bool   `json:"autostart"`
}

// UnmanagedDomains 列出某节点上尚未纳管的域。
//
// 「未纳管」的判定是**节点看到的域 − 控制面已有的记录**：节点不知道哪些
// 域有投影（那是控制面的账），控制面不知道节点上有什么（投影可能滞后），
// 两边各报各的、在这里合并，才是同一个时刻的完整答案。
func (s *Service) UnmanagedDomains(
	ctx context.Context, nodeID int64, v authz.Viewer,
) ([]UnmanagedDomain, error) {
	domains, err := s.scanDomains(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	// 只取名字：判定只需要「这个名字有没有记录」，把整行查回来是为了
	// 一个 bool，代价是白读几十列。
	var known []string
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ?", nodeID).
		Pluck("name", &known).Error; err != nil {
		log.Printf("[vm] 查询节点已有虚拟机记录失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	knownSet := make(map[string]bool, len(known))
	for _, n := range known {
		knownSet[n] = true
	}

	out := make([]UnmanagedDomain, 0, len(domains))
	for _, d := range domains {
		if knownSet[d.Name] {
			continue
		}
		out = append(out, UnmanagedDomain(d))
	}
	return out, nil
}

// AdoptDomain 把节点上的一个存量域登记到面板名下。
//
// 纳管**不改动域本身**：不关机、不迁盘、不改配置——虚拟化层里它是什么样，
// 纳管之后还是什么样。控制面只是补一份记录（状态、规格、归属），并把
// 节点回传的 UUID 记为对账键。此后它与其余虚拟机走完全相同的路径：
// 电源操作、监控、配额、审计。
type AdoptRequest struct {
	// Domain 是节点上的域名。必须是未纳管的——纳管已有记录的域没有入口
	// （那条路叫「误操作」）。
	Domain string
	// OwnerID 可选：纳管时直接指派归属；缺省为无主（管理员可再指派）。
	OwnerID *int64
	// Remark 可选备注。
	Remark *string
}

func (s *Service) AdoptDomain(
	ctx context.Context, nodeID int64, req AdoptRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	if req.Domain == "" {
		return nil, api.InvalidParameter("必须指定要纳管的域名")
	}

	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		return nil, err
	}

	// 以扫描结果为准，而不是直接信任请求里的名字：纳管一个不存在的域
	// 会造出一条永远「状态未知」的记录，而那看起来像节点出了问题。
	domains, err := s.scanDomains(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	var found *agent.DomainInfo
	for i := range domains {
		if domains[i].Name == req.Domain {
			found = &domains[i]
			break
		}
	}
	if found == nil {
		return nil, api.ValidationFailed(
			"节点上没有名为「" + req.Domain + "」的未纳管域。请刷新列表后重试——" +
				"它可能刚被别的会话纳管，或已从虚拟化层移除")
	}

	now := time.Now()
	row := model.VM{
		NodeID:    nodeID,
		Name:      found.Name,
		Status:    found.State,
		VCPU:      found.VCPU,
		MemoryMB:  found.MemoryMB,
		DiskGB:    found.DiskGB,
		CloneMode: model.CloneFull,
		OwnerID:   req.OwnerID,
		Remark:    req.Remark,
		Present:   true,
		// 纳管即一次对账：记录的规格来自刚刚扫描的定义，此刻就是新鲜的。
		LastSyncedAt: &now,
	}

	// 通知节点进入托管集合并取回 UUID。失败不回滚记录创建——域在节点上
	// 没有因为纳管发生任何变化，重试同样请求即可；先建记录可以让失败
	// 排查时有据可查（一条 present=true 但无 UUID 的行）。
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpVMAdopt, NodeID: nodeID, Target: found.Name,
	})
	if err != nil {
		log.Printf("[vm] 纳管指令未送达 node=%d domain=%s: %v", nodeID, found.Name, err)
	} else if result.Success {
		if info, ok := result.Data[agent.VMAdoptDataKey].(agent.VMAdoptInfo); ok && info.UUID != "" {
			uuid := info.UUID
			row.UUID = &uuid
		}
	}

	// 创建放在取 UUID 之后：一次 Create 带全字段，避免「先建再补」的两段
	// 写之间出现一条半记录（那段时间里列表查询会读到无 UUID 的行）。
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该域已被纳管（记录已存在），请刷新列表")
		}
		log.Printf("[vm] 创建纳管记录失败 node=%d domain=%s: %v", nodeID, found.Name, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		NodeID:       nodeID,
		ResourceType: "vm",
		ResourceID:   row.ID,
		ResourceName: row.Name,
		Action:       "vm.adopt",
		Params:       map[string]any{"domain": found.Name, "state": found.State},
		AfterState:   map[string]any{"vm_id": row.ID, "vcpu": found.VCPU, "memory_mb": found.MemoryMB},
		Success:      true,
		ClientIP:     clientIP,
	})

	view := toView(&row, nil, now, s.staleThreshold())
	return &view, nil
}

// scanDomains 向节点请求域列表。
func (s *Service) scanDomains(ctx context.Context, nodeID int64) ([]agent.DomainInfo, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNodeDomains, NodeID: nodeID,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法扫描虚拟化层的域列表")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	domains, _ := result.Data[agent.NodeDomainsDataKey].([]agent.DomainInfo)
	return domains, nil
}
