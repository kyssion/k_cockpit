// Package vm 实现虚拟机管理（F-2-01 ~ F-2-04）。
//
// 数据分两类（见 model.VM 的注释）：投影字段的权威在虚拟化层，元数据字段的
// 权威在控制面。本包负责读写这两类数据，而**真正的操作经任务队列下发给
// agent**——接口不等待执行完成（f-7-01 R-001）。
package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/model"
	"k_cockpit/internal/settings"
	"k_cockpit/internal/task"
)

// StaleThreshold 是投影数据的陈旧阈值。
//
// 超过该时长未与虚拟化层对账时，界面应提示「数据可能陈旧」——
// 把陈旧数据显示成当前状态，在排障时比没有数据更危险（f-2-01 Q-006）。
const StaleThreshold = 60 * time.Second

// namePattern 限定虚拟机名：与虚拟化层的命名约束保持一致。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

// Service 提供虚拟机领域操作。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	audit *audit.Recorder
	// agent 用于**实时探测**运行态：投影不得参与业务判定（f-2-01 R-002），
	// 因此受理写操作前必须向节点确认一次真实状态，而不是凭投影下结论。
	agent agent.Client
	// settings 用于读取可调整的运行参数（f-9-01）。为 nil 时使用内置默认值，
	// 这样单测与轻量部署不必先装配设置模块。
	settings settings.Provider
	// encKey 用于加解密控制台密码等可逆凭据（f-2-08）。
	encKey []byte
	// quota 校验存储配额（f-9-02）。**允许为 nil**：未启用配额的部署
	// 与绝大多数单测都不需要它，此时所有校验直接放行。
	quota QuotaChecker
	// computeQuota 校验计算资源配额（vCPU / 内存 / 实例数）。同为可选。
	computeQuota ComputeQuotaChecker
	// tags 批量读取标签，供列表使用。可为 nil。
	tags TagProvider

	// sessions 是控制台会话注册表。惰性创建：不用控制台的服务实例
	// 不必为此分配内存。
	sessions     *sessionRegistry
	sessionsOnce sync.Once
}

// SetEncryptionKey 设置可逆凭据的加密密钥。
func (s *Service) SetEncryptionKey(key []byte) {
	s.encKey = key
}

// ComputeQuotaChecker 校验计算资源配额（vCPU / 内存 / 实例数 / 快照 /
// 端口转发 / 公网 IP）。
//
// 用接口而不是直接依赖 computequota 包：配额是**可选能力**（未启用时不应
// 有任何行为变化），而把它做成必填的构造参数会让所有不关心配额的调用方
// （包括大批单测）都被迫构造一个空实现。
type ComputeQuotaChecker interface {
	// Check 校验能否再新增这些资源；超限返回可定位到维度的错误。
	Check(ctx context.Context, userID, nodeID int64, add computequota.Additions) error
	// SnapshotLimit 返回该用户在该节点上的快照上限；0 表示未配置，
	// 调用方退回自己的默认值。
	SnapshotLimit(ctx context.Context, userID, nodeID int64) (int, error)
}

// SetComputeQuota 装配计算配额校验器；不调用即不校验。
func (s *Service) SetComputeQuota(c ComputeQuotaChecker) {
	s.computeQuota = c
}

// NewService 构造虚拟机服务。
func NewService(
	db *gorm.DB, queue *task.Queue, recorder *audit.Recorder, client agent.Client,
	provider settings.Provider, quotaChecker QuotaChecker,
) *Service {
	return &Service{
		db: db, queue: queue, audit: recorder, agent: client,
		settings: provider, quota: quotaChecker,
	}
}

// QuotaChecker 校验存储配额（f-9-02）。
//
// 用接口而不是直接依赖 quota 包：配额是**可选能力**（未启用时不该有任何
// 行为变化），而把它做成必填的构造参数会让所有不关心配额的调用方都被迫
// 构造一个空实现。接口在这里只声明两个方法，quota.Service 恰好满足它。
type QuotaChecker interface {
	// Check 校验是否还有额度再新增 estimatedBytes 大小的资源。
	Check(ctx context.Context, userID, nodeID int64, estimatedBytes int64) error
	// CheckOverQuota 只校验「当前是否已经超出」。
	//
	// 用于**估算不出大小**的操作（如导出：产物多大取决于盘里真正写了多少
	// 数据）。这类操作在受理时无法知道结果大小，只能做一次较弱的检查。
	CheckOverQuota(ctx context.Context, userID, nodeID int64) error
}

// staleThreshold 返回当前的投影陈旧阈值。
//
// 从设置读取而不是直接用常量：这个值写进规格时是 60 秒，但不同部署环境
// 对「多久算陈旧」的容忍度不同——节点多、心跳慢的环境需要放宽它。
func (s *Service) staleThreshold() time.Duration {
	if s.settings == nil {
		return StaleThreshold
	}
	seconds := s.settings.Int(settings.KeyVMStaleThreshold, int(StaleThreshold.Seconds()))
	if seconds <= 0 {
		return StaleThreshold
	}
	return time.Duration(seconds) * time.Second
}

// View 是虚拟机的对外视图。
type View struct {
	ID      int64  `json:"id"`
	NodeID  int64  `json:"node_id"`
	Name    string `json:"name"`
	UUID    string `json:"uuid,omitempty"`
	OwnerID *int64 `json:"owner_id,omitempty"`

	Status    string `json:"status"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	IPSummary string `json:"ip_summary,omitempty"`

	Remark    string `json:"remark,omitempty"`
	GroupName string `json:"group_name,omitempty"`

	Present      bool       `json:"present"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	// Stale 提示投影数据可能已经过期，界面据此展示「数据可能陈旧」。
	Stale     bool      `json:"stale"`
	CreatedAt time.Time `json:"created_at"`

	// AvailableActions 是按**投影状态**算出的可用电源操作，供界面渲染按钮。
	//
	// 它可能与实际可用性不一致（投影滞后），此时后端会在受理请求时基于
	// 实时探测拒绝并说明原因。前端禁用只是体验优化，不构成安全边界
	// （f-2-01 R-004）。`stale` 为 true 时界面不应完全依赖它。
	AvailableActions []string `json:"available_actions"`

	// Firmware 是引导固件（bios / uefi），取值与配置矩阵同源。
	//
	// 下发它有两个具体用途：详情页显示"这台机器怎么启动"，以及**快照页
	// 据此决定是否显示「修复 UEFI 启动项」**——BIOS 机器没有启动项可修，
	// 给一个点了必然报错的按钮，只会让人以为修复失败了。
	Firmware string `json:"firmware,omitempty"`

	// HasConsole 表示该虚拟机是否有可用的控制台（display != none）。
	//
	// 为 false 时界面应隐藏控制台入口，而不是给一个点了打不开的按钮
	// （f-2-08 R-011）。
	HasConsole bool `json:"has_console"`

	// Tags 是该虚拟机的标签。**只在列表接口填充**——详情只有一台，由它的
	// 标签组件按需取更省一次查询。
	//
	// 列表里之所以要带它：标签是"这台机器是干什么的"的主要表达（分组之外
	// 唯一的自由度），看不到标签就无法在列表上筛选与辨认。填充方式是
	// **一次批量查询**（见 List），不是逐台关联——后者会让每次列表请求
	// 变成上百次数据库往返。
	Tags []string `json:"tags,omitempty"`

	// Usage 是最近一次采样的资源占用；没有采样时为 null。
	//
	// null 与 0 必须分开：0 看起来像"这台机器很闲"，而实际可能是"还没采
	// 到"或"机器已关机（采集器只采运行中的）"。
	Usage *UsageView `json:"usage,omitempty"`

	// Locked 表示该虚拟机被业务软锁保护（F-2-12），此时禁止删除。
	//
	// 它由**后端算好下发**，界面不自行判断：批量操作要提前提示「其中 N 台
	// 已锁定」（f-2-01 R-010），而前端的判断依据只能来自列表接口本身——
	// 让每个页面各自再查一次锁定状态，迟早会出现「界面上没标锁定、点删除
	// 却被拒绝」的不一致。
	Locked bool `json:"locked"`
	// LockReason 是加锁时填写的原因，供界面解释「为什么锁着」。
	LockReason string     `json:"lock_reason,omitempty"`
	LockedAt   *time.Time `json:"locked_at,omitempty"`

	// RescueActive 表示该虚拟机当前从救援镜像启动（F-2-12）。
	//
	// 界面据此显示醒目提示：救援模式下看到的系统**不是用户自己的系统**
	// （盘型、网卡、引导顺序都改过），把它当成日常状态会让人做出错误判断。
	RescueActive bool       `json:"rescue_active"`
	RescueSince  *time.Time `json:"rescue_since,omitempty"`

	// TemplateID / CloneMode 描述这台机器的来源（f-3-02）。
	//
	// 界面必须把它们显示出来：`linked` 的磁盘只是模板之上的一层覆盖，
	// **模板被删后数据就不可用了**，而且不会立刻报错。用户看不到这条依赖
	// 关系，就无法理解为什么「删掉一个模板」会让自己的机器出事。
	TemplateID *int64 `json:"template_id,omitempty"`
	CloneMode  string `json:"clone_mode,omitempty"`

	// HasReinstallBackup 表示重装留下了一份原系统盘备份，可以清理。
	//
	// **只下发有无，不下发路径**：路径是宿主机上的内部细节，暴露出去会诱使
	// 用户去宿主机上直接操作那个文件。界面需要知道的只是「有一份备份、可以
	// 清理」，以及「它挡着下一次重装」。
	HasReinstallBackup bool       `json:"has_reinstall_backup"`
	ReinstallAt        *time.Time `json:"reinstall_at,omitempty"`
}

// ListFilter 是列表查询条件。
type ListFilter struct {
	Status    string
	Keyword   string
	NodeID    int64
	GroupName string
	// SortBy 是排序字段，取值见 sortColumns；空表示按创建顺序（id 倒序）。
	SortBy string
	// Desc 为 true 时降序。
	Desc     bool
	Page     int
	PageSize int
	Viewer   authz.Viewer
}

// sortColumns 是允许排序的列。
//
// **白名单而不是直接拼接传入的字符串**：排序键出现在 ORDER BY 里，而那里
// 无法用参数绑定——把用户输入原样拼进去就是一处注入。白名单的另一半价值
// 是它明确了"哪些列可以排"，顺手也挡住了按一个没有索引的大字段排序。
var sortColumns = map[string]string{
	"name":       "name",
	"vcpu":       "vcpu",
	"memory":     "memory_mb",
	"disk":       "disk_gb",
	"ip":         "ip_summary",
	"created_at": "created_at",
}

// sortOrder 把排序条件转成 ORDER BY 子句。
func (f ListFilter) sortOrder() string {
	col, ok := sortColumns[f.SortBy]
	if !ok {
		// 默认按新建在前：列表页的第一屏通常是「刚建的那几台」。
		return "id DESC"
	}
	if f.Desc {
		return col + " DESC"
	}
	return col + " ASC"
}

// List 返回虚拟机列表与总数。
//
// 总数是**归属过滤后**的数量：返回全局总数会让 tenant 通过翻页差异推断出
// 他人有多少虚拟机（f-1-06 §5.2）。
func (s *Service) List(ctx context.Context, f ListFilter) ([]View, int64, error) {
	page, pageSize := normalizePage(f.Page, f.PageSize)

	query := s.db.WithContext(ctx).Model(&model.VM{})
	query = applyFilter(query, f)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		log.Printf("[vm] 统计虚拟机失败: %v", err)
		return nil, 0, api.Internal()
	}

	var vms []model.VM
	if err := query.Order(f.sortOrder()).
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&vms).Error; err != nil {
		log.Printf("[vm] 查询虚拟机失败: %v", err)
		return nil, 0, api.Internal()
	}

	now := time.Now()
	// 阈值只读一次：放在循环里会让每台虚拟机都触发一次设置查询，
	// 而列表页可能有上百台。
	threshold := s.staleThreshold()

	// 锁定状态同样**一次查完**：列表页上要标出哪些机器被锁着，
	// 逐个查会把一次列表请求变成上百次数据库往返。
	ids := make([]int64, 0, len(vms))
	for i := range vms {
		ids = append(ids, vms[i].ID)
	}
	locks, err := s.lockMap(ctx, ids)
	if err != nil {
		log.Printf("[vm] 查询锁定状态失败: %v", err)
		return nil, 0, api.Internal()
	}

	// 标签与最近占用**同样一次查完**，理由与锁定状态一致：列表页可能有
	// 上百台，逐个查会把一次请求变成上百次往返。
	//
	// 两者都只在**列表**里填：详情页由它自己的标签组件按需取，而详情
	// 只有一台，为它多一次查询没有意义。
	tags := map[int64][]string{}
	if s.tags != nil && len(ids) > 0 {
		if tags, err = s.tags.TagsOf(ctx, ids); err != nil {
			log.Printf("[vm] 查询标签失败: %v", err)
			return nil, 0, api.Internal()
		}
	}
	// 占用是**锦上添花**的一列，取不到就整列不显示，而不是让整个列表
	// 失败：列表是找机器的入口，为一行显示不出百分比而拒绝它，代价不对等。
	usage := map[int64]UsageView{}
	if len(ids) > 0 {
		var uerr error
		if usage, uerr = s.latestUsage(ctx, ids); uerr != nil {
			log.Printf("[vm] 查询最近占用失败（列表将不显示该列）: %v", uerr)
			usage = map[int64]UsageView{}
		}
	}

	views := make([]View, 0, len(vms))
	for i := range vms {
		view := toView(&vms[i], locks[vms[i].ID], now, threshold)
		if t, ok := tags[vms[i].ID]; ok && len(t) > 0 {
			view.Tags = t
		}
		if u, ok := usage[vms[i].ID]; ok {
			latest := u
			view.Usage = &latest
		}
		views = append(views, view)
	}
	return views, total, nil
}

// TagProvider 批量读取虚拟机的标签。
//
// 声明成接口而不是直接依赖 vmtag 包：列表只需要"给我这几台的标签"，
// 换出去之后测试不必构造一整套标签服务。
type TagProvider interface {
	TagsOf(ctx context.Context, vmIDs []int64) (map[int64][]string, error)
}

// SetTagProvider 装配标签来源。不调用时列表不带标签（详情页仍可编辑）。
func (s *Service) SetTagProvider(p TagProvider) { s.tags = p }

// UsageView 是一台虚拟机的**最近一次采样**占用。
//
// 它刻意**不来自实时探测**：列表一页 20 台，逐台探测就是 20 次跨节点往返
// ——那是真正意义的 N+1，而且是最贵的那种。采样最旧 60 秒，对一个"路过
// 看一眼"的列表来说完全够用；真要看此刻的数字，进详情页的监控。
type UsageView struct {
	CPUPercent float64 `json:"cpu_percent"`
	MemPercent float64 `json:"mem_percent"`
	MemUsedMB  int64   `json:"mem_used_mb"`
	// At 是采样时刻。没有它的百分比与"现在"无法区分，而列表上恰恰需要
	// 说明这一点——这是采样与实时的唯一区别。
	At string `json:"at"`
}

// latestUsage 一次取多台虚拟机的最近一条采样。
//
// 只查采样表而**不下发探测**：这里的目标是在列表上给出"大致在用什么"，
// 60 秒的延迟换掉 N 次网络往返是划算的。停机机器没有采样（采集器只采
// 运行中），因此结果里没有它——调用方按"缺失"处理，而不是当作 0%。
func (s *Service) latestUsage(ctx context.Context, vmIDs []int64) (map[int64]UsageView, error) {
	out := make(map[int64]UsageView, len(vmIDs))
	if len(vmIDs) == 0 {
		return out, nil
	}

	var rows []struct {
		VMID       int64     `gorm:"column:vm_id"`
		CPUPercent float64   `gorm:"column:cpu_percent"`
		MemPercent float64   `gorm:"column:mem_percent"`
		MemUsedMB  int64     `gorm:"column:mem_used_mb"`
		At         time.Time `gorm:"column:at"`
	}
	// 先取每台的最新时刻，再按 (vm_id, at) 取回那一条。
	//
	// 两步而不是窗口函数：ROW_NUMBER() 在 SQLite 与 PostgreSQL 上的可用
	// 版本不同，而这条查询是列表页的关键路径——用两种数据库都支持的写法
	// 比省一次查询重要。vm_id IN (...) 与 (vm_id, at) 索引都在。
	err := s.db.WithContext(ctx).Raw(`
		SELECT r.vm_id, r.cpu_percent, r.mem_percent, r.mem_used_mb, r.at
		FROM vm_stats_record r
		JOIN (
			SELECT vm_id, MAX(at) AS at FROM vm_stats_record WHERE vm_id IN ? GROUP BY vm_id
		) m ON r.vm_id = m.vm_id AND r.at = m.at`, vmIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].VMID] = UsageView{
			CPUPercent: rows[i].CPUPercent,
			MemPercent: rows[i].MemPercent,
			MemUsedMB:  rows[i].MemUsedMB,
			At:         rows[i].At.Format("2006-01-02T15:04:05Z07:00"),
		}
	}
	return out, nil
}

// Get 返回虚拟机详情；不属于当前视角的返回 404（不泄漏其是否存在）。
func (s *Service) Get(ctx context.Context, id int64, v authz.Viewer) (*View, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	lock, err := s.LockOf(ctx, id)
	if err != nil {
		return nil, err
	}

	view := toView(vm, lock, time.Now(), s.staleThreshold())
	return &view, nil
}

// load 按归属读取虚拟机。
//
// 不属于当前视角的返回 **404 而非 403**：403 会告诉调用方「这个 ID 确实
// 存在，只是你没权限」，从而可以被用来枚举他人资源（f-1-06 §5.2）。
func (s *Service) load(ctx context.Context, id int64, v authz.Viewer) (*model.VM, error) {
	query := s.db.WithContext(ctx).Where("id = ?", id)
	if !v.IsAdmin {
		query = query.Where("owner_id = ?", v.UserID)
	}

	var vm model.VM
	err := query.First(&vm).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("虚拟机不存在")
	case err != nil:
		log.Printf("[vm] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	return &vm, nil
}

// CreateRequest 是创建虚拟机的请求。
type CreateRequest struct {
	Name      string
	NodeID    int64
	VCPU      int
	MemoryMB  int
	DiskGB    int
	Remark    string
	GroupName string

	// TemplateID 非零表示从模板克隆（f-3-02）；为零表示从零安装。
	TemplateID int64
	// CloneMode 取值 full / linked（model.CloneFull / CloneLinked）。
	// 模板ID 非零时生效；留空时按 full 处理——**链式克隆必须是显式选择**，
	// 因为它引入了「父盘没了数据就没了」这个依赖，不该是默认行为。
	CloneMode string

	// --- 创建向导的其余配置（f-2-02）---
	//
	// 键名与 editFields 矩阵一致：创建与编辑共用同一份取值与范围定义，
	// 校验因此也走同一套（validateCreateConfig），不必再抄一份规则。

	DiskFormat      string
	DiskBus         string
	NicModel        string
	OSType          string
	MachineType     string
	Firmware        string
	SecureBoot      bool
	BootOrder       string
	AutoStart       bool
	Watchdog        string
	CPUType         string
	CPULimitPercent int
	APIC            *bool
	PAE             *bool
	FreezeOnStart   bool
	DiskIOPSTotal   int
	DiskIOPSRead    int
	DiskIOPSWrite   int

	// ISOFileID 非零表示创建成功后把该镜像挂到光驱——ISO 安装路径
	// （f-2-02）：虚拟机建好时空盘无法引导，挂上镜像才能进安装界面。
	ISOFileID int64
	// SwitchID 是主网口接入的交换机；为零表示落到节点的系统网络。
	SwitchID int64
	// SecurityGroupIDs 是主网口挂载的安全组。
	SecurityGroupIDs []int64

	// Count 是本次创建的台数（默认 1）。多台时 Name 作为前缀，
	// 逐台追加 -1 / -2 后缀。
	Count int
	// ClientToken 是前端生成的幂等键（f-2-02 Q-003）。
	ClientToken string
	// BatchKey 是批量分组键，便于在任务中心按批查看与取消。
	BatchKey string
}

// loadTemplateForClone 取出并校验要克隆的模板。
//
// 校验放在受理时而不是执行时：模板不可克隆是一个**当下的确定事实**，
// 排进队列等几分钟后再失败，用户会在收到通知时已经忘了自己点过什么。
func (s *Service) loadTemplateForClone(
	ctx context.Context, req CreateRequest, owner authz.Viewer,
) (*model.Template, error) {
	var tpl model.Template
	err := s.db.WithContext(ctx).Where("id = ?", req.TemplateID).First(&tpl).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("模板不存在")
	case err != nil:
		log.Printf("[vm] 查询模板失败: %v", err)
		return nil, api.Internal()
	}

	// 可见性：管理员不限；普通用户可用已发布的，或自己创建的私有模板。
	// 用 404 而非 403——403 会确认「这个 ID 存在」。
	if !owner.IsAdmin && !tpl.Published && (tpl.CreatedBy == nil || *tpl.CreatedBy != owner.UserID) {
		return nil, api.NotFound("模板不存在")
	}

	switch {
	case !tpl.IsReady():
		// 制备中与失败分开说：前者要等，后者要重建。都说成「不可用」
		// 会让用户在等待一个永远不会就绪的模板。
		if tpl.Status == model.TemplatePreparing {
			return nil, api.ValidationFailed("模板正在制备中，完成后才能用于创建虚拟机")
		}
		return nil, api.ValidationFailed("模板制备失败，请删除后重新制备")
	case !tpl.CloneEnabled:
		return nil, api.ValidationFailed("该模板已停止提供克隆")
	}

	// 模板是**节点内**资源：磁盘文件就在那个节点的存储池里。跨节点使用
	// 需要先导出再导入（f-2-14），而不是让它去挂载一个不存在的路径。
	if tpl.NodeID != req.NodeID {
		return nil, api.ValidationFailed(
			"模板与目标节点不在同一台宿主机上；跨节点使用需要先导出再导入")
	}

	return &tpl, nil
}

// maxCreateBatch 是一次创建的最大台数。
//
// 与批量克隆（f-3-02）取同一个上限、同一个理由：每台都要在存储上写一份
// 完整的镜像，同时进行的台数越多，宿主机的存储被占得越久——表现为**所有**
// 虚拟机的 IO 都变慢，而用户很难把它和「我刚才点了创建」联系起来。
const maxCreateBatch = 5

// Create 入队一个创建任务并立即返回。单台场景的便捷入口。
//
// **不等待创建完成**：创建虚拟机涉及磁盘镜像复制等耗时操作，同步等待会让
// 请求超时，也会让用户在界面上干等（f-7-01 R-001）。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, owner authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	tasks, err := s.CreateBatch(ctx, req, owner, operatorName, clientIP)
	if err != nil {
		return nil, err
	}
	return tasks[0], nil
}

// CreateBatch 入队一台或多台创建任务（f-2-02 R-007：N 台 = N 个独立任务）。
//
// 独立任务而不是「一个任务带 N 个子项」：单台失败不该影响其它台的结论，
// 用户也需要能单独取消某一台。共享的只有 BatchKey——它只用于分组展示。
func (s *Service) CreateBatch(
	ctx context.Context, req CreateRequest, owner authz.Viewer, operatorName, clientIP string,
) ([]*model.Task, error) {
	req.Name = strings.TrimSpace(req.Name)
	if !namePattern.MatchString(req.Name) {
		return nil, api.InvalidParameter("虚拟机名需为 1-63 位字母、数字或连字符，且以字母或数字开头")
	}
	if req.NodeID <= 0 {
		return nil, api.InvalidParameter("必须指定节点")
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > maxCreateBatch {
		return nil, api.InvalidParameter(
			"一次最多创建 " + strconv.Itoa(maxCreateBatch) + " 台：每台都要写入完整镜像，" +
				"同时进行会让宿主机存储持续占满，表现为所有虚拟机变慢")
	}
	if req.VCPU <= 0 || req.MemoryMB <= 0 || req.DiskGB <= 0 {
		return nil, api.InvalidParameter("CPU、内存与磁盘必须为正数")
	}
	if err := validateCreateConfig(req); err != nil {
		return nil, err
	}
	// 前置复核（R-001 / R-014）：打开向导时查过一次，到提交之间资源可能
	// 已经变化。不复查等于把几分钟前的结论当成事实。
	if err := s.ensureCreatePrerequisites(ctx, req.NodeID); err != nil {
		return nil, err
	}

	// 配额按**整批**校验（R-005）：逐台校验会让第 N 台在写入前才发现超额，
	// 而那时前面几台已经建好了——用户看到的是「建了一半」。
	if s.quota != nil {
		total := int64(req.DiskGB) * int64(req.Count) * 1024 * 1024 * 1024
		if err := s.quota.Check(ctx, owner.UserID, req.NodeID, total); err != nil {
			return nil, err
		}
	}
	// 计算资源配额：磁盘之外还要看核、内存与台数。它们与存储配额是**两类
	// 约束**（一个按周期累计、一个是存量），因此分开校验、分开报错。
	if s.computeQuota != nil {
		if err := s.computeQuota.Check(ctx, owner.UserID, req.NodeID, computequota.Additions{
			VMs:      req.Count,
			VCPU:     req.VCPU * req.Count,
			MemoryMB: req.MemoryMB * req.Count,
		}); err != nil {
			return nil, err
		}
	}

	// 模板在循环外解析一次：同一批用的是同一个模板，逐台回查既浪费、
	// 也会让「模板在第 3 台时被删」这种中间态产生前后不一致的结果。
	var tpl *model.Template
	if req.TemplateID > 0 {
		// 先归一化再传下去：loadTemplateForClone 收到的是**值拷贝**，
		// 在它里面改 req.CloneMode 不会影响这里——那样 params.CloneMode
		// 会一直是空串，而空串在下游会被当成「未指定」。
		if req.CloneMode == "" {
			req.CloneMode = model.CloneFull
		}
		if req.CloneMode != model.CloneFull && req.CloneMode != model.CloneLinked {
			return nil, api.InvalidParameter("克隆方式非法，可选 full 或 linked")
		}

		loaded, err := s.loadTemplateForClone(ctx, req, owner)
		if err != nil {
			return nil, err
		}
		tpl = loaded
	}
	// 磁盘不能小于模板自身：overlay 建在比父盘小的空间上会直接失败，
	// 而报错信息通常是一句「write beyond end of device」，从它出发
	// 几乎不可能定位到「你在创建时把磁盘调小了」。
	diskGB := req.DiskGB
	vcpu, memoryMB := req.VCPU, req.MemoryMB
	if tpl != nil {
		if diskGB < tpl.MinDiskGB {
			diskGB = tpl.MinDiskGB
		}
		if vcpu <= 0 {
			vcpu = tpl.DefaultCPU
		}
		if memoryMB <= 0 {
			memoryMB = tpl.DefaultMemoryMB
		}
	}

	tasks := make([]*model.Task, 0, req.Count)
	for i := 1; i <= req.Count; i++ {
		name := req.Name
		if req.Count > 1 {
			name = fmt.Sprintf("%s-%d", req.Name, i)
		}
		// 重名预检（R-010）：等到执行器撞唯一索引才失败，用户已经等了一轮
		// 任务调度；而且那时他拿到的只是一句「同名」，不知道该改成什么。
		if err := s.ensureNameAvailable(ctx, req.NodeID, name); err != nil {
			return nil, err
		}

		params := newCreateParams(req, name, vcpu, memoryMB, diskGB, tpl, owner.UserID)

		// 幂等键：有 client_token 就用它（同名不同次的创建意图也能区分），
		// 否则退回「同名同节点」——后者足以挡住最常见的重复点击。
		idempotencyKey := fmt.Sprintf("%s:%d:%s", model.TaskVMCreate, req.NodeID, name)
		if req.ClientToken != "" {
			idempotencyKey = fmt.Sprintf("vm.create:%s:%d", req.ClientToken, i)
		}

		t, err := s.queue.Enqueue(ctx, task.Spec{
			Type:           model.TaskVMCreate,
			NodeID:         req.NodeID,
			ResourceType:   "node",
			ResourceID:     req.NodeID,
			ResourceName:   name,
			OwnerID:        owner.UserID,
			CreatedBy:      owner.UserID,
			Params:         params,
			IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)

		s.record(ctx, audit.Entry{
			OperatorID:   owner.UserID,
			OperatorName: operatorName,
			NodeID:       req.NodeID,
			ResourceType: "vm",
			ResourceName: name,
			Action:       "vm.create.request",
			Params:       params,
			AfterState: map[string]any{
				"task_id": t.ID,
				// 批量键**只**在这里留痕：任务表里没有为它加列，而排查
				// 「这一批里哪几台是一起点出来的」靠的就是它。
				"batch_key": req.BatchKey,
			},
			Success:  true,
			ClientIP: clientIP,
		})
	}
	return tasks, nil
}

// ensureNameAvailable 校验节点内名称未被占用，冲突时给出可用建议名。
func (s *Service) ensureNameAvailable(ctx context.Context, nodeID int64, name string) error {
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND name = ?", nodeID, name).Count(&n).Error; err != nil {
		log.Printf("[vm] 校验重名失败 node=%d name=%s: %v", nodeID, name, err)
		return api.Internal()
	}
	if n == 0 {
		return nil
	}
	return api.Conflict("该节点上已存在同名虚拟机，建议改用「" + s.suggestName(ctx, nodeID, name) + "」")
}

// suggestName 在给定名称后追加序号，返回第一个未被占用的名字。
func (s *Service) suggestName(ctx context.Context, nodeID int64, base string) string {
	for i := 2; i < 100; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		var n int64
		if err := s.db.WithContext(ctx).Model(&model.VM{}).
			Where("node_id = ? AND name = ?", nodeID, candidate).Count(&n).Error; err != nil {
			break
		}
		if n == 0 {
			return candidate
		}
	}
	return base + "-new"
}

// validateCreateConfig 按**编辑矩阵**校验创建参数（f-2-02 R-003）。
//
// 规则取自 editFields 而不是在这里另写一份：创建与编辑允许的组合必须是同一
// 套，两处各写一遍的话，用户会在向导里选中一个编辑页不接受的取值，而错误
// 要等到保存时才出现。
func validateCreateConfig(req CreateRequest) error {
	selected := map[string]string{
		"disk_format":  req.DiskFormat,
		"disk_bus":     req.DiskBus,
		"nic_model":    req.NicModel,
		"os_type":      req.OSType,
		"machine_type": req.MachineType,
		"firmware":     req.Firmware,
		"boot_order":   req.BootOrder,
		"watchdog":     req.Watchdog,
		"cpu_type":     req.CPUType,
	}
	numbers := map[string]int{
		"cpu_limit_percent": req.CPULimitPercent,
		"disk_iops_total":   req.DiskIOPSTotal,
		"disk_iops_read":    req.DiskIOPSRead,
		"disk_iops_write":   req.DiskIOPSWrite,
	}

	for _, f := range editFields {
		if !f.InCreate {
			continue
		}
		if f.Kind == EditKindSelect {
			v := selected[f.Key]
			if v == "" {
				continue // 留空：由执行器与数据库默认值兜底
			}
			ok := false
			for _, o := range f.Options {
				if o.Value == v {
					ok = true
					break
				}
			}
			if !ok {
				return api.InvalidParameter(f.Label + "的取值不合法")
			}
		}
		if f.Kind == EditKindNumber {
			// 规格三件套不在这里校验：它们的最终取值要等模板推导之后
			// （模板会抬高磁盘下限、补齐 CPU 与内存），在此处按原始请求
			// 校验会把「由模板带出」的合法创建判为非法。
			if f.Key == "vcpu" || f.Key == "memory_mb" || f.Key == "disk_gb" {
				continue
			}
			n := numbers[f.Key]
			if n < f.Min || (f.Max > 0 && n > f.Max) {
				return api.InvalidParameter(
					f.Label + "需在 " + strconv.Itoa(f.Min) + " ~ " + strconv.Itoa(f.Max) + " 之间")
			}
		}
	}

	// IOPS 的「总量」与「读写分离」互斥（f-2-06）：同时给两组值会让用户
	// 以为两个都在生效，而实际只有一组被采用——那是静默的偏差。
	if req.DiskIOPSTotal > 0 && (req.DiskIOPSRead > 0 || req.DiskIOPSWrite > 0) {
		return api.InvalidParameter("IOPS 的总量限值与读写分离限值只能设一组")
	}
	return nil
}

// 磁盘处理方式（f-2-01 R-009）。
//
// 取值与 agent.DiskActionTransfer 等常量一致，这里重新声明是为了让 vm 包
// 不依赖 agent 包的常量名——它们分属控制面与节点两侧，各自演进时不必同步。
const (
	DiskActionDelete = "delete"
	DiskActionKeep   = "keep"
	// DiskActionTransfer 把磁盘文件搬回「我的存储 - 虚拟磁盘」。
	DiskActionTransfer = "transfer"
)

// Power 受理一次电源操作。
//
// 受理前做三件事，缺一不可：
//  1. **归属校验**——不属于当前视角的返回 404，不泄漏资源是否存在；
//  2. **维护模式校验**——维护模式的意义是「不再引入变更」（R-014）；
//  3. **实时探测 + 状态机校验**——投影可能滞后，凭它判断就可能在虚拟机
//     实际运行时执行危险操作（R-002 / R-004）。
//
// 通过后入队并立即返回：真正的执行由任务队列按资源锁串行（R-005），
// 同一虚拟机的并发电源操作不会交错。
//
// 刻意**不设幂等键**：连点两次「关机」会产生两个任务，第二个执行时因
// 状态已变而失败并说明原因。这是对的——用固定的幂等键（如
// `vm.power:{id}:start`）会把「开机→关机→再开机」中第三次开机与第一次
// 判为同一意图而静默丢弃。
func (s *Service) Power(
	ctx context.Context, id int64, rawAction string, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	action, err := ParsePowerAction(rawAction)
	if err != nil {
		return nil, err
	}

	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	if err := action.Validate(current); err != nil {
		return nil, err
	}

	params := powerParams{
		VMID:   vm.ID,
		VMName: vm.Name,
		Action: string(action),
		// 记录受理时的真实状态，便于事后区分「探测结果与投影不一致」
		// 与「执行时状态已变」两种情况。
		ObservedStatus: current,
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMPower,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		Action:       "vm.power.request",
		Params:       params,
		BeforeState:  map[string]any{"status": current},
		AfterState:   map[string]any{"task_id": t.ID},
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// DeleteRequest 是一次删除请求。
type DeleteRequest struct {
	// DiskAction 必填，取值 delete / keep / transfer。
	//
	// transfer 把磁盘文件搬回「我的存储 - 虚拟磁盘」：它是 keep 之外的一个
	// 真实选择——keep 只是把文件留在原地不删，那块盘会继续占着宿主机的
	// 空间，却不属于任何虚拟机，谁也看不见它。
	DiskAction string
}

// Delete 受理一次删除。
//
// 磁盘处理方式**必填且不做默认**（R-009）：默认值即「用户最可能接受的
// 选项」，若默认连盘删除，误操作代价是数据永久丢失；默认保留会累积无主
// 磁盘，但可以事后清理——两者相较取其轻，因此把选择交给用户显式做出。
func (s *Service) Delete(
	ctx context.Context, id int64, req DeleteRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	switch req.DiskAction {
	case DiskActionDelete, DiskActionKeep, DiskActionTransfer:
	default:
		return nil, api.InvalidParameter(
			"必须选择磁盘处理方式：delete（连同磁盘删除）、keep（保留磁盘）或 transfer（转移到我的存储）")
	}

	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	// 转移到「我的存储」需要一个归属：无主的机器没有"我的存储"可转，
	// 而默默地按保留处理会让用户以为磁盘已经进了自己的文件列表。
	if req.DiskAction == DiskActionTransfer && vm.OwnerID == nil {
		return nil, api.ValidationFailed("该虚拟机没有归属用户，无法把磁盘转移到「我的存储」，请选择保留或删除")
	}

	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 锁定检查放在最前面（f-2-01 R-010 / f-2-12）。
	//
	// 它是所有拒绝理由里**唯一一个持久且可自解**的：在途任务等一会儿就没了、
	// 运行态关个机就好，而锁必须由用户主动解开。先告诉他能立刻解决的那一条，
	// 比让他先等任务跑完、再发现还锁着要好。
	if err := s.ensureNotLocked(ctx, vm); err != nil {
		return nil, err
	}

	// 在途任务存在时拒绝删除，而不是排队（f-2-01 边界）。
	//
	// 与电源操作不同：电源操作排队是合理的（用户可能连续调整），而删除
	// 排在创建/开机后面执行，意味着「用户以为取消了的操作其实照样做了」。
	active, err := s.hasActiveTask(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	// 删除同样要探测：对运行中的虚拟机执行删除会强杀来宾进程并删除磁盘，
	// 代价不可逆。
	current, err := s.probeStatus(ctx, vm)
	if err != nil {
		return nil, err
	}
	switch current {
	case model.VMStatusRunning, model.VMStatusPaused, model.VMStatusSuspended:
		return nil, api.ValidationFailed(
			"虚拟机当前为" + DescribeStatus(current) + "，请先关机或强制断电后再删除",
		)
	}

	params := deleteParams{
		VMID:           vm.ID,
		VMName:         vm.Name,
		DiskAction:     req.DiskAction,
		ObservedStatus: current,
	}

	// 删除即**移入回收站**：记录保留、虚拟化层里还在（磁盘没动），只是从
	// 列表里消失。彻底删除是回收站里的第二步（见 Purge）。
	//
	// 记下删除时刻而不是靠 updated_at 推算：「什么时候删的」是回收站里唯一
	// 能帮用户判断"这是我上周误删的那台吗"的信息，而 updated_at 会被别的
	// 写操作刷新。
	now := time.Now()
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vm.ID).Update("deleted_at", now).Error; err != nil {
		log.Printf("[vm] 记录删除时刻失败 id=%d: %v", vm.ID, err)
		// 不阻断：删除本身已经受理，缺一个时刻只是回收站里少一列显示。
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDelete,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      ownerOf(vm, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID:   v.UserID,
		OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		Action:       "vm.delete.request",
		Params:       params,
		BeforeState:  map[string]any{"status": current},
		AfterState:   map[string]any{"task_id": t.ID, "disk_action": req.DiskAction},
		Success:      true,
		ClientIP:     clientIP,
	})
	return t, nil
}

// probeStatus 向节点**实时探测**虚拟机的真实运行态。
//
// 投影不可用于业务判定（R-002），因此每个写操作受理前都要走这一趟。
// 探测失败一律拒绝操作：无法确认状态时不猜，猜错的方向可能是对运行中的
// 虚拟机断电。
func (s *Service) probeStatus(ctx context.Context, vm *model.VM) (string, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMStatus,
		NodeID: vm.NodeID,
		Target: vm.Name,
	})
	if err != nil {
		return "", api.Unavailable("节点不可达，无法确认虚拟机当前状态")
	}
	if !result.Success {
		return "", api.Unavailable("无法确认虚拟机当前状态")
	}

	status, _ := result.Data[agent.StatusDataKey].(string)
	if status == "" {
		return "", api.Unavailable("节点未返回虚拟机状态")
	}
	return status, nil
}

// ensureNodeUsable 校验节点存在且未处于维护模式。
func (s *Service) ensureNodeUsable(ctx context.Context, nodeID int64) error {
	var n model.Node
	err := s.db.WithContext(ctx).
		Select("id", "maintenance_mode").
		Where("id = ?", nodeID).
		First(&n).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return api.ValidationFailed("虚拟机的所属节点已不存在")
	case err != nil:
		log.Printf("[vm] 查询节点失败: %v", err)
		return api.Internal()
	}
	if n.MaintenanceMode {
		return api.ValidationFailed("节点处于维护模式，已暂停创建与电源操作")
	}
	return nil
}

// hasActiveTask 报告该虚拟机是否有在途任务。
//
// unknown 也算在途：它等待节点重连后对账收敛，而不是已经结束。
func (s *Service) hasActiveTask(ctx context.Context, vmID int64) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&model.Task{}).
		Where("resource_type = ? AND resource_id = ?", "vm", vmID).
		Where("status IN ?", []string{model.TaskPending, model.TaskRunning, model.TaskUnknown}).
		Count(&count).Error
	if err != nil {
		log.Printf("[vm] 统计在途任务失败: %v", err)
		return false, api.Internal()
	}
	return count > 0, nil
}

// ownerOf 返回新任务应记录的归属。
//
// 管理员代他人操作时沿用资源原有的归属，否则会把别人的虚拟机「过户」给自己
// ——那是权限提升的一个入口（f-1-06 R-007）。
func ownerOf(vm *model.VM, v authz.Viewer) int64 {
	if vm.OwnerID != nil {
		return *vm.OwnerID
	}
	return v.UserID
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

func applyFilter(query *gorm.DB, f ListFilter) *gorm.DB {
	// 已不在虚拟化层的记录默认不显示，但保留在库中供审计与历史引用。
	query = query.Where("present = ?", true)

	if f.Status != "" {
		query = query.Where("status = ?", f.Status)
	}
	if f.NodeID > 0 {
		query = query.Where("node_id = ?", f.NodeID)
	}
	if f.GroupName != "" {
		query = query.Where("group_name = ?", f.GroupName)
	}
	if f.Keyword != "" {
		// 转义 LIKE 通配符：否则用户输入一个 % 就会匹配到全部记录，
		// 看起来像「搜索功能坏了」。
		keyword := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(f.Keyword)
		// 用 LOWER(...) LIKE LOWER(...) 而非 ILIKE：后者是 PostgreSQL 专有语法，
		// 在 SQLite（单元测试与轻量部署）上会直接报错——查询行为会随驱动而变，
		// 而这类差异只会在切换数据库时才暴露。
		query = query.Where("LOWER(name) LIKE LOWER(?)", "%"+keyword+"%")
	}
	// 归属过滤强制注入：漏加一处就是一次越权。
	if !f.Viewer.IsAdmin {
		query = query.Where("owner_id = ?", f.Viewer.UserID)
	}
	return query
}

// toView 构造对外视图。
//
// lock 允许为 nil（未加锁，且多数虚拟机连记录都没有）。
func toView(vm *model.VM, lock *model.VMLock, now time.Time, threshold time.Duration) View {
	view := View{
		ID:           vm.ID,
		NodeID:       vm.NodeID,
		Name:         vm.Name,
		OwnerID:      vm.OwnerID,
		Status:       vm.Status,
		VCPU:         vm.VCPU,
		MemoryMB:     vm.MemoryMB,
		DiskGB:       vm.DiskGB,
		Present:      vm.Present,
		LastSyncedAt: vm.LastSyncedAt,
		Stale:        vm.IsStale(now, threshold),
		CreatedAt:    vm.CreatedAt,
		// 按投影状态给出可用动作。投影滞后时可能与实际不符，后端受理时
		// 会以实时探测为准重新校验（f-2-01 R-004）。
		AvailableActions: AvailableActions(vm.Status),
		HasConsole:       vm.HasConsole(),
		Firmware:         vm.Firmware,
		RescueActive:     vm.RescueActive,
		RescueSince:      vm.RescueSince,
		TemplateID:       vm.TemplateID,
		CloneMode:        vm.CloneMode,
		ReinstallAt:      vm.ReinstallAt,
		// 空串与 nil 都算「没有备份」：两种写法都可能出现（列可空、历史数据
		// 可能是空串），让调用方去分辨只会让每个使用点都写一遍判断。
		HasReinstallBackup: vm.ReinstallBackup != nil && *vm.ReinstallBackup != "",
	}
	if vm.UUID != nil {
		view.UUID = *vm.UUID
	}
	if vm.IPSummary != nil {
		view.IPSummary = *vm.IPSummary
	}
	if vm.Remark != nil {
		view.Remark = *vm.Remark
	}
	if vm.GroupName != nil {
		view.GroupName = *vm.GroupName
	}
	// lock 允许为 nil：绝大多数虚拟机没有加过锁，连一行记录都没有。
	// 让调用方去构造一个空记录只会在每个调用点重复同一段判断。
	if lock.IsLocked() {
		view.Locked = true
		view.LockReason = derefStr(lock.Reason)
		view.LockedAt = lock.LockedAt
	}
	return view
}

func normalizePage(page, pageSize int) (int, int) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return page, pageSize
}
