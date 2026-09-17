// Package quota 实现按用户按节点的存储配额（F-9-02）。
//
// 一条贯穿本包的设计：**用量现算，不读缓存**。
//
// user_storage.used_bytes 是一列缓存，但它只用于「列出所有用户的用量」
// 这类批量展示。任何**判断**（能不能再建一台、能不能再导一次）都必须现算——
// 缓存可能比真实值旧，而按一个偏小的旧值放行会让用户悄悄超额，按一个偏大的
// 旧值拦截会让用户莫名其妙地被拒。两种情况都比慢一点糟。
package quota

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// Service 提供配额计算与校验。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
}

// NewService 构造配额服务。
func NewService(db *gorm.DB, recorder *audit.Recorder) *Service {
	return &Service{db: db, audit: recorder}
}

// Usage 是一个用户在某个节点上的用量明细。
//
// 分项返回而不是只给一个总数：用户看到「超出配额 200 GB」时的第一个问题是
// 「是什么占了这么多」，而只给一个总数等于让他自己去翻。
type Usage struct {
	UserID int64 `json:"user_id"`
	NodeID int64 `json:"node_id"`

	// VMDisksBytes 是名下虚拟机磁盘的合计（按**配置大小**算）。
	//
	// 用配置大小而不是实际占用：qcow2 是稀疏文件，一台配了 500 GB 但只用了
	// 20 GB 的机器实际占 20 GB。但配额要按**承诺**算——否则用户可以为 100 台
	// 机器各配 500 GB，实际占 0，等到真写满时才发现盘根本放不下。
	VMDisksBytes int64 `json:"vm_disks_bytes"`
	// ExportsBytes 是导出产物的合计（按**实际大小**算）。
	//
	// 这里用实际大小而不是配置值：产物是一次性的确定文件，它占多少就是多少，
	// 没有「将来会长大」这回事。
	ExportsBytes int64 `json:"exports_bytes"`
	// TemplatesBytes 是名下模板的合计（按配置大小算，理由同虚拟机磁盘）。
	TemplatesBytes int64 `json:"templates_bytes"`

	TotalBytes int64 `json:"total_bytes"`
	// QuotaBytes 为 0 表示不限制。
	QuotaBytes int64 `json:"quota_bytes"`
	// Unlimited 为 true 时 RemainingBytes 为 -1。
	Unlimited bool `json:"unlimited"`
	// RemainingBytes 为 -1 表示不限制。
	RemainingBytes int64 `json:"remaining_bytes"`
	// OverQuota 表示已经超出——它决定的是「能不能再新增」，而不是
	// 「已用的东西怎么办」（那属于运维决策，不在本包范围内）。
	OverQuota bool `json:"over_quota"`
	ReadOnly  bool `json:"read_only"`
}

const (
	// bPerGB 是换算常量。
	bPerGB = 1 << 30
	// estimateOverhead 是导出产物的估算系数。
	//
	// 导出的实际大小**在受理时无法知道**（取决于磁盘里真正写了多少数据），
	// 因此只能用一个上界去估算。取 1.1 而不是 1.0：OVA 还包含 OVF 描述与
	// 校验清单，虽然占比很小，但把「估算值恰好在边界上」这种情况推向拒绝
	// 一侧比推向放行一侧安全——放行之后超额，用户已经拿到了产物。
	estimateOverhead = 1.1
)

// Usage 计算某用户在某节点上的实际用量。
//
// 三个来源都取自**控制面自己的表**：虚拟机、导出、模板。不需要问节点——
// 那些数据本来就是控制面维护的投影，而配额判断必须基于控制面认可的事实。
func (s *Service) Usage(ctx context.Context, userID, nodeID int64) (*Usage, error) {
	usage := &Usage{UserID: userID, NodeID: nodeID}

	// 虚拟机磁盘：按配置大小（GB → 字节）。
	//
	// 只算 present 的记录：已不在虚拟化层的虚拟机不再占用磁盘（要么被删了，
	// 要么早就没了），把它们的配置大小继续算进配额，用户会看到一份永远
	// 降不下来的用量。
	var vmGB int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("owner_id = ? AND node_id = ? AND present = ?", userID, nodeID, true).
		Select("COALESCE(SUM(disk_gb), 0)").Scan(&vmGB).Error; err != nil {
		log.Printf("[quota] 统计虚拟机磁盘失败: %v", err)
		return nil, api.Internal()
	}
	usage.VMDisksBytes = vmGB * bPerGB

	// 导出产物：按实际大小。只算成功且未被删除的。
	var exports int64
	if err := s.db.WithContext(ctx).Model(&model.VMExport{}).
		Where("created_by = ? AND node_id = ? AND status = ?", userID, nodeID, model.ExportSuccess).
		Select("COALESCE(SUM(size_bytes), 0)").Scan(&exports).Error; err != nil {
		log.Printf("[quota] 统计导出产物失败: %v", err)
		return nil, api.Internal()
	}
	usage.ExportsBytes = exports

	// 模板：按配置大小。
	var tplGB int64
	if err := s.db.WithContext(ctx).Model(&model.Template{}).
		Where("created_by = ? AND node_id = ?", userID, nodeID).
		Select("COALESCE(SUM(disk_size_gb), 0)").Scan(&tplGB).Error; err != nil {
		log.Printf("[quota] 统计模板失败: %v", err)
		return nil, api.Internal()
	}
	usage.TemplatesBytes = tplGB * bPerGB

	usage.TotalBytes = usage.VMDisksBytes + usage.ExportsBytes + usage.TemplatesBytes

	row, err := s.loadStorage(ctx, userID, nodeID)
	if err != nil {
		return nil, err
	}
	usage.QuotaBytes = row.QuotaBytes
	usage.Unlimited = row.IsUnlimited()
	usage.ReadOnly = row.ReadOnly
	usage.RemainingBytes = row.Remaining(usage.TotalBytes)
	usage.OverQuota = !row.IsUnlimited() && usage.TotalBytes > row.QuotaBytes

	s.refreshCache(ctx, userID, nodeID, usage.TotalBytes, row)
	return usage, nil
}

// Check 校验是否有额度再新增 estimatedBytes 大小的资源。
//
// 返回 nil 表示放行。**没有配额记录也放行**：未设配额的用户不受限制，
// 这是面板的默认状态——先能用，再谈限额。
func (s *Service) Check(ctx context.Context, userID, nodeID int64, estimatedBytes int64) error {
	usage, err := s.Usage(ctx, userID, nodeID)
	if err != nil {
		return err
	}

	if usage.ReadOnly {
		return api.ValidationFailed("你在该节点上的存储空间处于只读状态，无法新建资源")
	}
	if usage.Unlimited {
		return nil
	}

	// 判断用的是「当前用量 + 本次估算」而不是「当前是否已超」。
	//
	// 只判「是否已超」会放行一次明显超额的创建，等它落地后才发现——那时
	// 用户已经等了几分钟，而拒绝的成本远高于一开始就拦住。
	projected := usage.TotalBytes + estimatedBytes
	if projected > usage.QuotaBytes {
		return api.ValidationFailed(
			"存储配额不足：已用 " + humanGB(usage.TotalBytes) +
				"，本次需要约 " + humanGB(estimatedBytes) +
				"，配额 " + humanGB(usage.QuotaBytes))
	}
	return nil
}

// CheckOverQuota 只校验「当前是否已经超出」。
//
// 用于那些**估算不出大小**的操作（如导出：产物多大取决于盘里真正写了多少
// 数据）。这类操作只能做一次较弱的检查——已经超额就不再放行，未超额则允许，
// 产物落地后可能小幅超出，界面上如实显示。
//
// 这不是妥协，而是这类操作的固有边界：**在受理时无法知道结果大小**。
// 假装能算准只会给出一个看起来精确、实际误导的数字。
func (s *Service) CheckOverQuota(ctx context.Context, userID, nodeID int64) error {
	usage, err := s.Usage(ctx, userID, nodeID)
	if err != nil {
		return err
	}
	if usage.ReadOnly {
		return api.ValidationFailed("你在该节点上的存储空间处于只读状态，无法新建资源")
	}
	if usage.OverQuota {
		return api.ValidationFailed(
			"存储配额已超出（已用 " + humanGB(usage.TotalBytes) +
				"，配额 " + humanGB(usage.QuotaBytes) +
				"），请先清理一些导出产物或删除不再使用的虚拟机")
	}
	return nil
}

// SetQuotaRequest 是管理员设置配额的请求。
type SetQuotaRequest struct {
	UserID     int64
	NodeID     int64
	Enabled    bool
	QuotaBytes int64
	ReadOnly   bool
}

// Set 设置配额（管理员）。
//
// **同步生效**：它是纯控制面元数据，不影响宿主机上的任何东西。做成任务会
// 制造一个「界面说配额改了、实际还没改」的窗口。
func (s *Service) Set(
	ctx context.Context, req SetQuotaRequest, operatorID int64, operatorName, clientIP string,
) (*Usage, error) {
	if req.UserID <= 0 || req.NodeID <= 0 {
		return nil, api.InvalidParameter("必须指定用户与节点")
	}
	if req.QuotaBytes < 0 {
		return nil, api.InvalidParameter("配额不能为负数")
	}

	var row model.UserStorage
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND node_id = ?", req.UserID, req.NodeID).
		First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		row = model.UserStorage{UserID: req.UserID, NodeID: req.NodeID}
	case err != nil:
		log.Printf("[quota] 查询配额失败: %v", err)
		return nil, api.Internal()
	}

	before := map[string]any{
		"enabled": row.Enabled, "quota_bytes": row.QuotaBytes, "read_only": row.ReadOnly,
	}
	row.Enabled = req.Enabled
	row.QuotaBytes = req.QuotaBytes
	row.ReadOnly = req.ReadOnly

	// Save 的语义：有主键则更新，无则插入。
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		log.Printf("[quota] 保存配额失败: %v", err)
		return nil, api.Internal()
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: operatorID, OperatorName: operatorName,
			NodeID: req.NodeID, ResourceType: "user_storage", ResourceID: req.UserID,
			Action:      "quota.set",
			BeforeState: before,
			AfterState: map[string]any{
				"enabled": req.Enabled, "quota_bytes": req.QuotaBytes, "read_only": req.ReadOnly,
			},
			Success: true, ClientIP: clientIP,
		})
	}

	return s.Usage(ctx, req.UserID, req.NodeID)
}

// UsageForUser 返回某用户在所有节点上的用量。
//
// 节点列表取自**已有配额的节点 + 该用户已经有资源的节点**：只按配额记录去
// 查会漏掉「没设配额但用了很多」的用户（那恰恰是最需要被看到的），只按资源
// 去查又会漏掉「设了配额但还没用」的节点。
func (s *Service) UsageForUser(ctx context.Context, userID int64) ([]*Usage, error) {
	nodeIDs := map[int64]bool{}

	var quotaNodes []int64
	if err := s.db.WithContext(ctx).Model(&model.UserStorage{}).
		Where("user_id = ?", userID).Pluck("node_id", &quotaNodes).Error; err != nil {
		log.Printf("[quota] 查询配额节点失败: %v", err)
		return nil, api.Internal()
	}
	for _, id := range quotaNodes {
		nodeIDs[id] = true
	}

	var vmNodes []int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("owner_id = ? AND present = ?", userID, true).
		Distinct().Pluck("node_id", &vmNodes).Error; err != nil {
		log.Printf("[quota] 查询虚拟机节点失败: %v", err)
		return nil, api.Internal()
	}
	for _, id := range vmNodes {
		nodeIDs[id] = true
	}

	out := make([]*Usage, 0, len(nodeIDs))
	for id := range nodeIDs {
		usage, err := s.Usage(ctx, userID, id)
		if err != nil {
			return nil, err
		}
		out = append(out, usage)
	}
	// 按节点排序：map 的遍历顺序是随机的，不排序会让同一用户在两次请求里
	// 看到不同的排列，而列表的顺序变化会让人以为数据变了。
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// List 返回全部配额记录（管理员总览）。
//
// 这里读的是**缓存值**，同时刷新它——批量页面不需要逐行现算，而用户看到的
// 是自己那一刻的用量概览，差一次操作的量级可以接受。判断仍然走现算。
func (s *Service) List(ctx context.Context, nodeID int64) ([]*Usage, error) {
	var rows []model.UserStorage
	query := s.db.WithContext(ctx).Order("user_id ASC")
	if nodeID > 0 {
		query = query.Where("node_id = ?", nodeID)
	}
	if err := query.Find(&rows).Error; err != nil {
		log.Printf("[quota] 查询配额列表失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]*Usage, 0, len(rows))
	for i := range rows {
		usage, err := s.Usage(ctx, rows[i].UserID, rows[i].NodeID)
		if err != nil {
			return nil, err
		}
		out = append(out, usage)
	}
	return out, nil
}

// loadStorage 读取配额记录；不存在时返回一个零值记录（= 不限）。
func (s *Service) loadStorage(ctx context.Context, userID, nodeID int64) (*model.UserStorage, error) {
	var row model.UserStorage
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND node_id = ?", userID, nodeID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 没有记录 = 未设配额 = 不限。**不给默认限额**：面板的默认状态应当
		// 是「先能用」，限额由管理员显式设置。
		return &model.UserStorage{UserID: userID, NodeID: nodeID}, nil
	}
	if err != nil {
		log.Printf("[quota] 查询配额记录失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

// refreshCache 把现算出来的用量写回缓存列。
//
// 写回失败**不影响结果**：缓存只是加速展示，判断已经用现算值做完了。
func (s *Service) refreshCache(ctx context.Context, userID, nodeID, used int64, row *model.UserStorage) {
	if row.ID == 0 {
		// 记录还不存在就别为「刷新缓存」建一条：那会让「未设配额」与
		// 「配额为空」在表里变得难以区分，而报表会多出一堆无意义的行。
		return
	}
	if row.UsedBytes == used {
		return
	}
	if err := s.db.WithContext(ctx).Model(&model.UserStorage{}).
		Where("id = ?", row.ID).Update("used_bytes", used).Error; err != nil {
		log.Printf("[quota] 刷新用量缓存失败 user=%d node=%d: %v", userID, nodeID, err)
	}
}

// humanGB 把字节数格式化成可读的 GB 描述。
//
// 与前端各自的格式化分开：这句文案会直接出现在错误消息里（接口返回给用户），
// 因此必须在服务端生成——前端拿到的是成品，不必再去解析一个数字。
func humanGB(bytes int64) string {
	if bytes == 0 {
		return "0 GB"
	}
	gb := float64(bytes) / float64(bPerGB)
	if gb < 1 {
		return trimZero(gb*1024) + " MB"
	}
	return trimZero(gb) + " GB"
}

// trimZero 保留一位小数，整数时不带小数点。
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}

// EstimateExportBytes 估算一次导出的产物大小。
//
// 用**虚拟磁盘配置大小 × 一个系数**作为上界。它一定不小于真实产物吗？
// 不一定——但真实大小取决于盘里写了多少数据，在受理时**根本无法知道**。
//
// 因此这个估算只用于「给用户一个参考量级」，**不用于拒绝**：按一个可能
// 高估的值拒绝一次合法导出，比让产物小幅超出更让人难以接受。真正的拦截
// 走 CheckOverQuota（已经超额就不再放行）。
func EstimateExportBytes(diskGB int, includeDataDisks bool) int64 {
	base := float64(diskGB) * estimateOverhead
	if includeDataDisks {
		// 数据盘大小不在受理参数里，给一个保守的加量提示而不是猜一个数字。
		base *= 1.5
	}
	return int64(base * bPerGB)
}
