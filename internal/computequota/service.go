// Package computequota 实现按用户按节点的**计算资源**配额：vCPU / 内存 /
// 实例数。
//
// 与存储配额（quota 包）和周期型配额（quotaenforce 包）的区别是本包存在的
// 理由：它们是"用了多少"，而这里是"此刻占着多少"。因此：
//
//   - 没有周期，上限长期有效；
//   - 超限不"处置"存量机器（不关机、不限速），而是**拒绝新建**——把一台
//     正在跑的机器限速掉，比不让它再建一台严重得多。
package computequota

import (
	"context"
	"errors"
	"log"
	"strconv"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// Service 提供计算配额的设置与校验。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
}

// NewService 构造服务。
func NewService(db *gorm.DB, recorder *audit.Recorder) *Service {
	return &Service{db: db, audit: recorder}
}

// Usage 是一个用户在一个节点上的当前占用。
//
// 只统计 present = true 的机器：虚拟化层已经不存在的那些不该继续占着
// 用户的额度——它们通常意味着一次失败的删除，而用户正准备重建。
type Usage struct {
	UserID     int64 `json:"user_id"`
	NodeID     int64 `json:"node_id"`
	VCPU       int   `json:"vcpu"`
	MemoryMB   int   `json:"memory_mb"`
	VMCount    int   `json:"vm_count"`
	HasQuota   bool  `json:"has_quota"`
	QuotaVCPU  int   `json:"quota_vcpu"`
	QuotaMemMB int   `json:"quota_memory_mb"`
	QuotaVMs   int   `json:"quota_vm_count"`
}

// View 是配额与用量的合体，供界面直接渲染。
type View struct {
	Usage
	Username string `json:"username,omitempty"`
}

// List 列出某节点上全部（有配额或有用量的）用户。
func (s *Service) List(ctx context.Context, nodeID int64) ([]View, error) {
	quotas, err := s.quotasOfNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	usages, err := s.usagesOfNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	names, err := s.userNames(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	// 并集：有配额但一台机器都没有的用户也要列出来，否则管理员看不到
	// "我给谁配过、他还没用"。
	seen := map[int64]bool{}
	out := make([]View, 0, len(quotas)+len(usages))
	for _, q := range quotas {
		seen[q.UserID] = true
		v := View{Usage: Usage{
			UserID: q.UserID, NodeID: nodeID, HasQuota: true,
			QuotaVCPU: q.VCPU, QuotaMemMB: q.MemoryMB, QuotaVMs: q.VMCount,
		}}
		if u, ok := usages[q.UserID]; ok {
			v.VCPU, v.MemoryMB, v.VMCount = u.VCPU, u.MemoryMB, u.VMCount
		}
		v.Username = names[q.UserID]
		out = append(out, v)
	}
	for uid, u := range usages {
		if seen[uid] {
			continue
		}
		v := View{Usage: u}
		v.Username = names[uid]
		out = append(out, v)
	}
	return out, nil
}

// Set 设置配额。三个上限全为 0 表示删除这条配额（回到不限）。
func (s *Service) Set(
	ctx context.Context, nodeID, userID int64, vcpu, memoryMB, vmCount int,
	operatorID int64, operatorName, clientIP string,
) error {
	if nodeID <= 0 || userID <= 0 {
		return api.InvalidParameter("必须指定节点与用户")
	}
	if vcpu < 0 || memoryMB < 0 || vmCount < 0 {
		return api.InvalidParameter("上限不能为负数")
	}

	if vcpu == 0 && memoryMB == 0 && vmCount == 0 {
		if err := s.db.WithContext(ctx).
			Where("node_id = ? AND user_id = ?", nodeID, userID).
			Delete(&model.ComputeQuota{}).Error; err != nil {
			log.Printf("[computequota] 删除配额失败 node=%d user=%d: %v", nodeID, userID, err)
			return api.Internal()
		}
		s.record(ctx, audit.Entry{
			OperatorID: operatorID, OperatorName: operatorName,
			NodeID: nodeID, ResourceType: "compute_quota", ResourceName: userText(userID),
			Action: "compute_quota.delete", Success: true, ClientIP: clientIP,
		})
		return nil
	}

	row := model.ComputeQuota{
		NodeID: nodeID, UserID: userID,
		VCPU: vcpu, MemoryMB: memoryMB, VMCount: vmCount,
	}
	// 用主键冲突时更新的方式而不是先查后写：两步之间可能已经有人建了同一
	// 条记录，而那时的报错是一句看不懂的唯一约束冲突。
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ?", nodeID, userID).
		Assign(model.ComputeQuota{VCPU: vcpu, MemoryMB: memoryMB, VMCount: vmCount}).
		FirstOrCreate(&row).Error
	if err != nil {
		log.Printf("[computequota] 写入配额失败 node=%d user=%d: %v", nodeID, userID, err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "compute_quota", ResourceName: userText(userID),
		Action: "compute_quota.set",
		Params: map[string]any{
			"vcpu": vcpu, "memory_mb": memoryMB, "vm_count": vmCount,
		},
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Check 校验能否再新增这些资源；超限返回**可列出维度与余量**的错误。
//
// 余量必须给出来：只说"超出配额"会让用户去猜是哪个维度超了、差多少，
// 而他此时正卡在创建这一步上。
func (s *Service) Check(
	ctx context.Context, userID, nodeID int64, addVMs, addVCPU, addMemoryMB int,
) error {
	var q model.ComputeQuota
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ?", nodeID, userID).First(&q).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil // 没有配额 = 不限
	case err != nil:
		log.Printf("[computequota] 查询配额失败: %v", err)
		// 读不到配额时**放行**而不是拦住：配额是管理手段，数据库抖动不该
		// 让用户建不了机器。这里的选择与存储配额一致。
		return nil
	}

	u, err := s.usageOf(ctx, nodeID, userID)
	if err != nil {
		return err
	}

	// 逐项检查并**一次报全**：用户改完 vCPU 再被内存拦一次，会以为系统在
	// 刁难他，而实际上两个维度都超。
	var problems []string
	if q.VCPU > 0 && u.VCPU+addVCPU > q.VCPU {
		problems = append(problems, dimensionText(
			"vCPU", u.VCPU, addVCPU, q.VCPU, "核"))
	}
	if q.MemoryMB > 0 && u.MemoryMB+addMemoryMB > q.MemoryMB {
		problems = append(problems, dimensionText(
			"内存", u.MemoryMB, addMemoryMB, q.MemoryMB, "MB"))
	}
	if q.VMCount > 0 && u.VMCount+addVMs > q.VMCount {
		problems = append(problems, dimensionText(
			"虚拟机数量", u.VMCount, addVMs, q.VMCount, "台"))
	}
	if len(problems) > 0 {
		return api.ValidationFailed("超出计算资源配额：" + joinText(problems))
	}
	return nil
}

func (s *Service) usageOf(ctx context.Context, nodeID, userID int64) (Usage, error) {
	// 列名必须显式声明：GORM 的命名策略把 `VCPU` 转成 `v_cpu`，而这里扫描的
	// 别名是 `vcpu`。不写标签的结果是这一列**永远是 0**——不报错、看起来像
	// "没占额度"，与 model/vm.go 里 VCPU 那一处是同一个坑。
	var row struct {
		VCPU     int `gorm:"column:vcpu"`
		MemoryMB int `gorm:"column:memory_mb"`
		Count    int `gorm:"column:count"`
	}
	err := s.db.WithContext(ctx).Model(&model.VM{}).
		Select(`COALESCE(SUM(vcpu), 0) AS vcpu,
			COALESCE(SUM(memory_mb), 0) AS memory_mb,
			COUNT(*) AS count`).
		Where("node_id = ? AND owner_id = ? AND present = ?", nodeID, userID, true).
		Scan(&row).Error
	if err != nil {
		log.Printf("[computequota] 统计占用失败 node=%d user=%d: %v", nodeID, userID, err)
		return Usage{}, api.Internal()
	}
	return Usage{
		UserID: userID, NodeID: nodeID,
		VCPU: row.VCPU, MemoryMB: row.MemoryMB, VMCount: row.Count,
	}, nil
}

func (s *Service) usagesOfNode(ctx context.Context, nodeID int64) (map[int64]Usage, error) {
	var rows []struct {
		OwnerID  int64 `gorm:"column:owner_id"`
		VCPU     int   `gorm:"column:vcpu"`
		MemoryMB int   `gorm:"column:memory_mb"`
		Count    int   `gorm:"column:count"`
	}
	err := s.db.WithContext(ctx).Model(&model.VM{}).
		Select(`owner_id,
			COALESCE(SUM(vcpu), 0) AS vcpu,
			COALESCE(SUM(memory_mb), 0) AS memory_mb,
			COUNT(*) AS count`).
		Where("node_id = ? AND present = ?", nodeID, true).
		Group("owner_id").Scan(&rows).Error
	if err != nil {
		log.Printf("[computequota] 统计节点占用失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	out := make(map[int64]Usage, len(rows))
	for i := range rows {
		if rows[i].OwnerID == 0 {
			continue // 无归属的机器不占任何人的额度
		}
		out[rows[i].OwnerID] = Usage{
			UserID: rows[i].OwnerID, NodeID: nodeID,
			VCPU: rows[i].VCPU, MemoryMB: rows[i].MemoryMB, VMCount: rows[i].Count,
		}
	}
	return out, nil
}

func (s *Service) quotasOfNode(ctx context.Context, nodeID int64) ([]model.ComputeQuota, error) {
	var rows []model.ComputeQuota
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).
		Order("user_id").Find(&rows).Error; err != nil {
		log.Printf("[computequota] 查询配额失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	return rows, nil
}

func (s *Service) userNames(ctx context.Context, nodeID int64) (map[int64]string, error) {
	ids := map[int64]bool{}
	var quotas []model.ComputeQuota
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&quotas).Error; err != nil {
		return nil, api.Internal()
	}
	for i := range quotas {
		ids[quotas[i].UserID] = true
	}
	var vms []model.VM
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND present = ?", nodeID, true).
		Select("owner_id").Find(&vms).Error; err != nil {
		return nil, api.Internal()
	}
	for i := range vms {
		if vms[i].OwnerID != nil {
			ids[*vms[i].OwnerID] = true
		}
	}
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	var users []model.User
	if err := s.db.WithContext(ctx).Where("id IN ?", list).Find(&users).Error; err != nil {
		log.Printf("[computequota] 查询用户名失败: %v", err)
		return map[int64]string{}, nil
	}
	out := make(map[int64]string, len(users))
	for i := range users {
		out[users[i].ID] = users[i].Username
	}
	return out, nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

func dimensionText(name string, used, add, limit int, unit string) string {
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return name + "已用 " + itoa(used) + " " + unit +
		"，本次再要 " + itoa(add) + " " + unit +
		"，上限 " + itoa(limit) + " " + unit +
		"（还剩 " + itoa(remaining) + " " + unit + "）"
}

func joinText(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "；"
		}
		out += p
	}
	return out
}

func userText(id int64) string { return "user:" + strconv.FormatInt(id, 10) }

func itoa(n int) string { return strconv.Itoa(n) }
