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
	UserID   int64 `json:"user_id"`
	NodeID   int64 `json:"node_id"`
	VCPU     int   `json:"vcpu"`
	MemoryMB int   `json:"memory_mb"`
	VMCount  int   `json:"vm_count"`
	// 数量型占用：该用户在这个节点上**所有**虚拟机的快照、端口转发与
	// 公网 IP 之和，而不是单台机器的——配额是给用户的，用户换一台机器建
	// 快照不该绕过去。
	Snapshots    int `json:"snapshots"`
	PortForwards int `json:"port_forwards"`
	PublicIPs    int `json:"public_ips"`

	HasQuota          bool `json:"has_quota"`
	QuotaVCPU         int  `json:"quota_vcpu"`
	QuotaMemMB        int  `json:"quota_memory_mb"`
	QuotaVMs          int  `json:"quota_vm_count"`
	QuotaSnapshots    int  `json:"quota_snapshots"`
	QuotaPortForwards int  `json:"quota_port_forwards"`
	QuotaPublicIPs    int  `json:"quota_public_ips"`
}

// Additions 是一次操作要新增的量，未涉及的两个维度留 0。
//
// 用结构而不是六个位置参数：调用方（创建虚拟机、加转发、绑公网 IP）各自
// 只关心其中一两项，位置参数会让他们写出 Check(0, 0, 0, 1, 0) 这种看不出
// 在加什么的代码，而写错一位的表现是"校验了错误的维度"。
type Additions struct {
	VMs          int
	VCPU         int
	MemoryMB     int
	Snapshots    int
	PortForwards int
	PublicIPs    int
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
			QuotaSnapshots: q.Snapshots, QuotaPortForwards: q.PortForwards,
			QuotaPublicIPs: q.PublicIPs,
		}}
		if u, ok := usages[q.UserID]; ok {
			v.VCPU, v.MemoryMB, v.VMCount = u.VCPU, u.MemoryMB, u.VMCount
			v.Snapshots, v.PortForwards, v.PublicIPs = u.Snapshots, u.PortForwards, u.PublicIPs
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

// Limits 是六个上限，全为 0 表示删除这条配额（回到不限）。
type Limits struct {
	VCPU         int
	MemoryMB     int
	VMCount      int
	Snapshots    int
	PortForwards int
	PublicIPs    int
}

// zero 报告六个上限是否全为 0（即"取消配额"）。
func (l Limits) zero() bool {
	return l.VCPU == 0 && l.MemoryMB == 0 && l.VMCount == 0 &&
		l.Snapshots == 0 && l.PortForwards == 0 && l.PublicIPs == 0
}

// Set 设置配额。六个上限全为 0 表示删除这条配额（回到不限）。
//
// 参数从三个位置参数改成结构：维度数量翻倍之后，Set(node, user, 8, 16, 5,
// 0, 0, 0) 这样的调用已经无法阅读——谁也说不清第 6 个 0 是快照还是转发。
func (s *Service) Set(
	ctx context.Context, nodeID, userID int64, limits Limits,
	operatorID int64, operatorName, clientIP string,
) error {
	if nodeID <= 0 || userID <= 0 {
		return api.InvalidParameter("必须指定节点与用户")
	}
	if limits.VCPU < 0 || limits.MemoryMB < 0 || limits.VMCount < 0 ||
		limits.Snapshots < 0 || limits.PortForwards < 0 || limits.PublicIPs < 0 {
		return api.InvalidParameter("上限不能为负数")
	}

	if limits.zero() {
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

	assign := model.ComputeQuota{
		NodeID: nodeID, UserID: userID,
		VCPU: limits.VCPU, MemoryMB: limits.MemoryMB, VMCount: limits.VMCount,
		Snapshots: limits.Snapshots, PortForwards: limits.PortForwards,
		PublicIPs: limits.PublicIPs,
	}
	// 用主键冲突时更新的方式而不是先查后写：两步之间可能已经有人建了同一
	// 条记录，而那时的报错是一句看不懂的唯一约束冲突。
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ?", nodeID, userID).
		Assign(assign).FirstOrCreate(&assign).Error
	if err != nil {
		log.Printf("[computequota] 写入配额失败 node=%d user=%d: %v", nodeID, userID, err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "compute_quota", ResourceName: userText(userID),
		Action: "compute_quota.set",
		Params: map[string]any{
			"vcpu": limits.VCPU, "memory_mb": limits.MemoryMB, "vm_count": limits.VMCount,
			"snapshots": limits.Snapshots, "port_forwards": limits.PortForwards,
			"public_ips": limits.PublicIPs,
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
	ctx context.Context, userID, nodeID int64, add Additions,
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
	if q.VCPU > 0 && u.VCPU+add.VCPU > q.VCPU {
		problems = append(problems, dimensionText(
			"vCPU", u.VCPU, add.VCPU, q.VCPU, "核"))
	}
	if q.MemoryMB > 0 && u.MemoryMB+add.MemoryMB > q.MemoryMB {
		problems = append(problems, dimensionText(
			"内存", u.MemoryMB, add.MemoryMB, q.MemoryMB, "MB"))
	}
	if q.VMCount > 0 && u.VMCount+add.VMs > q.VMCount {
		problems = append(problems, dimensionText(
			"虚拟机数量", u.VMCount, add.VMs, q.VMCount, "台"))
	}
	if q.Snapshots > 0 && u.Snapshots+add.Snapshots > q.Snapshots {
		problems = append(problems, dimensionText(
			"快照数量", u.Snapshots, add.Snapshots, q.Snapshots, "个"))
	}
	if q.PortForwards > 0 && u.PortForwards+add.PortForwards > q.PortForwards {
		problems = append(problems, dimensionText(
			"端口转发", u.PortForwards, add.PortForwards, q.PortForwards, "条"))
	}
	if q.PublicIPs > 0 && u.PublicIPs+add.PublicIPs > q.PublicIPs {
		problems = append(problems, dimensionText(
			"公网 IP", u.PublicIPs, add.PublicIPs, q.PublicIPs, "个"))
	}
	if len(problems) > 0 {
		return api.ValidationFailed("超出计算资源配额：" + joinText(problems))
	}
	return nil
}

// SnapshotLimit 返回某台虚拟机允许的最大快照数。
//
// 配额里配了就用配额（按用户 × 节点），没配就退回编译期默认值。这样
// "没配配额"的部署行为与从前完全一致——新维度不该悄悄改变既有系统的
// 边界，那会让一次升级变成一次意外。
func (s *Service) SnapshotLimit(ctx context.Context, userID, nodeID int64) (int, error) {
	var q model.ComputeQuota
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ?", nodeID, userID).First(&q).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return 0, nil // 0 = 使用调用方的默认值
	case err != nil:
		log.Printf("[computequota] 查询配额失败: %v", err)
		return 0, api.Internal()
	}
	return q.Snapshots, nil
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

	// 数量型维度按"该用户在这台节点上的全部虚拟机"汇总统计。
	//
	// 三个 COUNT 而不是一次连表：它们的基数都不大（一个用户的快照与转发
	// 通常以十计），而一次三路 LEFT JOIN 的写法在空结果时的语义（0 还是
	// NULL）要靠 COALESCE 逐个兜住，读起来比三次查询难得多。
	counts, err := s.countsOf(ctx, nodeID, userID)
	if err != nil {
		return Usage{}, err
	}
	return Usage{
		UserID: userID, NodeID: nodeID,
		VCPU: row.VCPU, MemoryMB: row.MemoryMB, VMCount: row.Count,
		Snapshots: counts.Snapshots, PortForwards: counts.PortForwards,
		PublicIPs: counts.PublicIPs,
	}, nil
}

// counts 是数量型维度的占用。
type counts struct {
	Snapshots    int
	PortForwards int
	PublicIPs    int
}

// countsOf 统计某用户在某节点上的快照 / 端口转发 / 公网 IP 数量。
func (s *Service) countsOf(ctx context.Context, nodeID, userID int64) (counts, error) {
	vmIDs, err := s.vmIDsOf(ctx, nodeID, userID)
	if err != nil {
		return counts{}, err
	}
	out := counts{}
	if len(vmIDs) == 0 {
		return out, nil
	}
	count := func(table any) (int64, error) {
		var n int64
		if err := s.db.WithContext(ctx).Model(table).
			Where("vm_id IN ?", vmIDs).Count(&n).Error; err != nil {
			return 0, err
		}
		return n, nil
	}
	snapshots, err := count(&model.VMSnapshot{})
	if err != nil {
		log.Printf("[computequota] 统计快照失败: %v", err)
		return counts{}, api.Internal()
	}
	forwards, err := count(&model.PortForward{})
	if err != nil {
		log.Printf("[computequota] 统计端口转发失败: %v", err)
		return counts{}, api.Internal()
	}
	// 公网 IP 只数**未释放**的绑定：历史绑定留着是为了追溯，把它们算进
	// "你还占着几个地址"会让用户刚解绑就仍然超限。
	var ips int64
	if err := s.db.WithContext(ctx).Model(&model.PublicIPBinding{}).
		Where("vm_id IN ? AND released_at IS NULL", vmIDs).Count(&ips).Error; err != nil {
		log.Printf("[computequota] 统计公网 IP 失败: %v", err)
		return counts{}, api.Internal()
	}
	out.Snapshots, out.PortForwards, out.PublicIPs = int(snapshots), int(forwards), int(ips)
	return out, nil
}

// vmIDsOf 取该用户在该节点上的虚拟机 ID 列表。
func (s *Service) vmIDsOf(ctx context.Context, nodeID, userID int64) ([]int64, error) {
	var ids []int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND owner_id = ? AND present = ?", nodeID, userID, true).
		Pluck("id", &ids).Error; err != nil {
		log.Printf("[computequota] 查询虚拟机列表失败: %v", err)
		return nil, api.Internal()
	}
	return ids, nil
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

	// 数量型维度**按节点一次算完**，而不是对每个用户各查一遍：这个接口
	// 是给管理员看全表的，逐用户查询会把 N 个用户变成 3N 次往返。
	grouped, err := s.countsOfNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for uid, c := range grouped {
		u, ok := out[uid]
		if !ok {
			// 一台机器都没有、却仍占着快照/转发的用户（机器刚被删完）
			// 也要出现：否则管理员看到的"已用 12 个快照"没有归属。
			u = Usage{UserID: uid, NodeID: nodeID}
		}
		u.Snapshots, u.PortForwards, u.PublicIPs = c.Snapshots, c.PortForwards, c.PublicIPs
		out[uid] = u
	}
	return out, nil
}

// countsOfNode 按归属用户汇总某节点上的数量型占用。
func (s *Service) countsOfNode(ctx context.Context, nodeID int64) (map[int64]counts, error) {
	out := make(map[int64]counts)

	rows := func(table string, extra string) ([]struct {
		OwnerID int64 `gorm:"column:owner_id"`
		Count   int   `gorm:"column:count"`
	}, error) {
		var res []struct {
			OwnerID int64 `gorm:"column:owner_id"`
			Count   int   `gorm:"column:count"`
		}
		err := s.db.WithContext(ctx).Table(table).
			Select("vm.owner_id AS owner_id, COUNT(*) AS count").
			Joins("JOIN vm ON vm.id = "+table+".vm_id").
			Where("vm.node_id = ? AND vm.present = ?"+extra, nodeID, true).
			Group("vm.owner_id").Scan(&res).Error
		return res, err
	}

	snapRows, err := rows("vm_snapshot", "")
	if err != nil {
		log.Printf("[computequota] 汇总快照失败: %v", err)
		return nil, api.Internal()
	}
	for _, r := range snapRows {
		c := out[r.OwnerID]
		c.Snapshots = r.Count
		out[r.OwnerID] = c
	}

	pfRows, err := rows("port_forward", "")
	if err != nil {
		log.Printf("[computequota] 汇总端口转发失败: %v", err)
		return nil, api.Internal()
	}
	for _, r := range pfRows {
		c := out[r.OwnerID]
		c.PortForwards = r.Count
		out[r.OwnerID] = c
	}

	ipRows, err := rows("public_ip_binding", " AND public_ip_binding.released_at IS NULL")
	if err != nil {
		log.Printf("[computequota] 汇总公网 IP 失败: %v", err)
		return nil, api.Internal()
	}
	for _, r := range ipRows {
		c := out[r.OwnerID]
		c.PublicIPs = r.Count
		out[r.OwnerID] = c
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
