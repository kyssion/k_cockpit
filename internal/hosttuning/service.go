// Package hosttuning 实现宿主机性能调优（KSM / ZRAM / 嵌套虚拟化 / CPU 亲和）。
//
// 本包的一条原则：**开关与效果一起给**。
//
// 只显示「已启用」的话，用户无法回答两个最实际的问题：该不该开、开了有没有用。
// 而 KSM 尤其如此——它是一个**持续消耗 CPU** 的机制，在什么都没合并的时候
// 照样扫描内存。因此"开着但一无所获"是一个真实存在、且用户完全看不出来的
// 坏状态。要让它可见，就必须把收益数字与扫描轮次一起给出来。
//
// 第二处：**代价要写出来**。KSM 与 ZRAM 的开关看起来一样，而代价完全不同
// （前者持续吃 CPU 换去重，后者吃 CPU 换压缩内存），压缩算法之间也是（lz4
// 快而压缩率低，zstd 反之）。把这些藏起来，用户只能凭感觉选。
package hosttuning

import (
	"context"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Service 提供调优能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder}
}

// View 是调优状态的对外视图。
type View struct {
	NodeID int64             `json:"node_id"`
	KSM    agent.KSMState    `json:"ksm"`
	ZRAM   agent.ZRAMState   `json:"zram"`
	Nested agent.NestedState `json:"nested"`
	// Items 是每一项的说明与代价（供界面渲染，避免前端硬编码第二份）。
	Items []Item `json:"items"`
	// Presets 是 CPU 亲和性预设。
	Presets []PresetView `json:"presets"`
}

// Item 是一项调优的元数据。
//
// **由后端下发而不是前端硬编码**：代价说明与"这项有没有效果"的口径属于
// 业务知识，写在前端会与实现漂移——而漂移之后界面上那句"代价很小"可能正
// 好描述着一个已经很贵的机制。
type Item struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Benefit 说明它带来什么。
	Benefit string `json:"benefit"`
	// Cost 说明它的代价——**这一项不能省**。
	Cost string `json:"cost"`
	// Metric 说明怎么判断它有没有用。
	Metric string `json:"metric"`
}

// Items 返回调优项的元数据。
func Items() []Item {
	return []Item{
		{
			Key: "ksm", Label: "内核同页合并（KSM）",
			Benefit: "把内容相同的内存页合并成一份。同一镜像开多台虚拟机时省得最多——它们的内存里往往有大段完全一样的内容。",
			Cost:    "**持续消耗 CPU**：它在什么都没合并的时候照样扫描内存。CPU 本来就吃紧的机器上，开它可能让整体变慢。",
			Metric:  "看「已省内存」与「扫描轮次」：轮次很多而收益是 0，说明这台机器上没有可合并的页，应当关掉。",
		},
		{
			Key: "zram", Label: "ZRAM 压缩内存",
			Benefit: "在内存里划一块压缩区当交换用，把不常用的页压起来。比交换到磁盘快得多，也不用担心磁盘 IO。",
			Cost:    "**吃 CPU**（压缩与解压），而且它挤占的是**同一块物理内存**——分配 4GB 给 ZRAM 就少了 4GB 可用内存，只是那 4GB 里的内容被压缩后占得更少。",
			Metric:  "看压缩率（原始数据 ÷ 实际占用）与换出的量。压缩率低于 2 倍时，它占的内存可能不如直接用掉划算。",
		},
		{
			Key: "nested", Label: "嵌套虚拟化",
			Benefit: "允许在虚拟机里再跑虚拟机（KVM in KVM）。做容器/虚拟化开发、或在虚拟机里跑 Docker Desktop 时需要它。",
			Cost:    "宿主的虚拟化扩展会被暴露给来宾。**安全性上略有降低**，且少数情况下会影响宿主自身的性能。",
			Metric:  "只在确实需要时开。没有这个需求时它只是多暴露了一层。",
		},
	}
}

// Get 读取调优状态。
func (s *Service) Get(ctx context.Context, nodeID int64) (*View, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostTuning, NodeID: nodeID,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取调优状态")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	view := &View{NodeID: nodeID, Items: Items(), Presets: []PresetView{}}
	if st, ok := result.Data[agent.TuningStateKey].(agent.TuningState); ok {
		view.KSM, view.ZRAM, view.Nested = st.KSM, st.ZRAM, st.Nested
	}

	presets, err := s.ListPresets(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	view.Presets = presets
	return view, nil
}

// Request 是修改调优的请求。
type Request struct {
	NodeID int64
	// Item 取 ksm / zram / nested。
	Item string
	// Enabled 用于开关类。
	Enabled *bool
	// ZRAM 专用。
	DisksizeMB int64
	Algorithm  string
	MemLimitMB int64
}

// Apply 修改一项调优配置。
func (s *Service) Apply(
	ctx context.Context, req Request, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	switch req.Item {
	case "ksm", "zram", "nested":
	default:
		return nil, api.InvalidParameter("未知的调优项")
	}
	if req.Item == "zram" {
		if err := validateZRAM(req); err != nil {
			return nil, err
		}
	}

	params := map[string]any{"item": req.Item, "enabled": req.Enabled}
	if req.Item == "zram" {
		params["disksize_mb"] = req.DisksizeMB
		params["algorithm"] = req.Algorithm
		params["mem_limit_mb"] = req.MemLimitMB
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskHostTuning,
		NodeID:       req.NodeID,
		ResourceType: "host_tuning",
		ResourceID:   req.NodeID,
		ResourceName: req.Item,
		OwnerID:      v.UserID, CreatedBy: v.UserID,
		Params: params,
	})
	if err != nil {
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "host_tuning",
		ResourceName: req.Item, Action: "host_tuning.apply",
		Params:  params,
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// validateZRAM 校验 ZRAM 参数。
//
// 压缩算法与容量都有实际边界，而越界的表现在用户看来是"虚拟机跑起来很慢"
// 或者"内存莫名其妙少了一块"——与刚才那个操作联系不起来。
func validateZRAM(req Request) error {
	if req.DisksizeMB > 0 && req.DisksizeMB < 128 {
		return api.InvalidParameter("ZRAM 容量至少 128 MB——比这更小的压缩区基本起不到作用")
	}
	if req.DisksizeMB > 1024*1024 {
		return api.InvalidParameter("ZRAM 容量不能超过 1 TB——请确认单位是 MB，不是 GB")
	}
	switch strings.ToLower(req.Algorithm) {
	case "", "lz4", "zstd", "lzo", "lz4hc":
	default:
		return api.InvalidParameter("不支持的压缩算法（可用：lz4 / zstd / lzo / lz4hc）")
	}
	if req.MemLimitMB < 0 {
		return api.InvalidParameter("内存阈值不能为负")
	}
	return nil
}

// --- CPU 亲和性预设 ---

// PresetView 是一条 CPU 亲和性预设。
type PresetView struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	// CPUSet 是绑定的物理核（如 "0-3"、"0,2,4"）。
	CPUSet string `json:"cpuset"`
	// CPUSetDesc 解释这串 cpuset 对应哪些核、以及它意味着什么。
	CPUSetDesc string `json:"cpuset_desc"`
	Remark     string `json:"remark,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// PresetRequest 是创建预设的请求。
type PresetRequest struct {
	NodeID int64
	Name   string
	CPUSet string
	Remark string
}

// ListPresets 返回节点上的亲和性预设。
func (s *Service) ListPresets(ctx context.Context, nodeID int64) ([]PresetView, error) {
	var rows []model.CPUAffinityPreset
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).Order("id ASC").Find(&rows).Error; err != nil {
		log.Printf("[hosttuning] 查询预设失败: %v", err)
		return nil, api.Internal()
	}
	out := make([]PresetView, 0, len(rows))
	for i := range rows {
		out = append(out, toPresetView(&rows[i]))
	}
	return out, nil
}

// CreatePreset 新建亲和性预设。
func (s *Service) CreatePreset(
	ctx context.Context, req PresetRequest, v authz.Viewer, operatorName, clientIP string,
) (*PresetView, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, api.InvalidParameter("必须填写预设名称")
	}
	if len(name) > 64 {
		return nil, api.InvalidParameter("预设名称最长 64 字")
	}
	// **cpuset 是会被拼进 libvirt 配置的字符串**，因此它的格式必须校验：
	// 一个带着分号的 cpuset 会让域配置解析失败，而报错来自 libvirt、
	// 与"名称里多打了一个字符"联系不起来。
	if err := validateCPUSet(req.CPUSet); err != nil {
		return nil, err
	}

	row := model.CPUAffinityPreset{
		NodeID: req.NodeID, Name: name, CPUSet: strings.TrimSpace(req.CPUSet),
	}
	if req.Remark != "" {
		row.Remark = &req.Remark
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicate(err) {
			return nil, api.Conflict("该节点下已有同名预设")
		}
		log.Printf("[hosttuning] 创建预设失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "cpu_affinity_preset",
		ResourceID: row.ID, ResourceName: name,
		Action:  "cpu_affinity_preset.create",
		Params:  map[string]any{"cpuset": row.CPUSet},
		Success: true, ClientIP: clientIP,
	})
	view := toPresetView(&row)
	return &view, nil
}

// DeletePreset 删除预设。
func (s *Service) DeletePreset(
	ctx context.Context, nodeID, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	var row model.CPUAffinityPreset
	if err := s.db.WithContext(ctx).
		Where("id = ? AND node_id = ?", id, nodeID).First(&row).Error; err != nil {
		return api.NotFound("预设不存在")
	}
	if err := s.db.WithContext(ctx).Delete(&model.CPUAffinityPreset{}, row.ID).Error; err != nil {
		return api.Internal()
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "cpu_affinity_preset",
		ResourceID: row.ID, ResourceName: row.Name,
		Action:  "cpu_affinity_preset.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// validateCPUSet 校验 cpuset 表达式。
//
// 只允许数字、逗号与连字符。**黑白名单同时用**：黑名单防已知的注入写法，
// 白名单防还没想到的那些——而这个字符串最终会进 libvirt 的域配置，
// 少一种可表达的写法就少一整类绕过方式（与目录共享只收相对路径同理）。
func validateCPUSet(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return api.InvalidParameter("必须填写 CPU 集合，例如 0-3 或 0,2,4")
	}
	if len(s) > 128 {
		return api.InvalidParameter("CPU 集合最长 128 字符")
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == ',', r == '-':
		default:
			return api.InvalidParameter("CPU 集合只能包含数字、逗号与连字符（例如 0-3 或 0,2,4）")
		}
	}
	return nil
}

func toPresetView(p *model.CPUAffinityPreset) PresetView {
	v := PresetView{
		ID: p.ID, NodeID: p.NodeID, Name: p.Name, CPUSet: p.CPUSet,
		CPUSetDesc: describeCPUSet(p.CPUSet),
		CreatedAt:  p.CreatedAt.Format(time.RFC3339),
	}
	if p.Remark != nil {
		v.Remark = *p.Remark
	}
	return v
}

// describeCPUSet 把 cpuset 翻译成人话。
//
// 「0-3,8」这种写法要用户在脑子里展开，而展开错了的后果是"我明明绑了 4 个核
// 却只用了 2 个"——那种问题不会报错，只表现为性能不如预期。
func describeCPUSet(s string) string {
	parts := strings.Split(s, ",")
	total := 0
	desc := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.Index(p, "-"); i > 0 {
			lo, hi := atoiSafe(p[:i]), atoiSafe(p[i+1:])
			if hi >= lo {
				total += hi - lo + 1
				desc = append(desc, p+"（共 "+itoa(hi-lo+1)+" 核）")
				continue
			}
		}
		total++
		desc = append(desc, p)
	}
	if total == 0 {
		return ""
	}
	return strings.Join(desc, "；") + " · 合计 " + itoa(total) + " 个核"
}

func atoiSafe(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return 0
		}
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func isDuplicate(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unique") || strings.Contains(s, "duplicate")
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}
