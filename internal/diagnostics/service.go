// Package diagnostics 按分类导出排障包（F-9-03）。
//
// 三条设计决定，每一条都与"这个包会被发给别人"有关：
//
//  1. **脱敏发生在打包时，而不是提醒用户自己检查。**
//     排障包的用途就是发给技术支持或贴到某个地方去。任何依赖用户"记得先
//     把密码删掉"的安排都会失败——他会直接发出去。因此敏感字段在生成的
//     那一刻就被打码，用户拿到的东西本身就是可以外发的。
//
//  2. **脱敏清单只有一份，而且是复用现有的。**
//     设置项是否敏感由 settings.Spec.Secret 决定，而 settings.Service.List
//     已经对敏感项返回空值并置 IsSet（R-009）。这里直接用它。
//     自己再维护一份"哪些键是敏感的"清单，迟早会漏掉新增的那一项——而
//     漏掉的表现在是"某个密钥被原样导出"，且没有任何地方会报错。
//
//  3. **同步生成、不落盘。**
//     诊断包是**瞬时快照**，没有理由把它留在磁盘上：不落盘就不存在"忘记
//     删"。这与抓包不同——抓包必须落盘，因为它要在节点上跑一段时间；而
//     诊断包是"此刻读一遍然后打包"，天生可以边生成边发走。
//
// 包里会有一个 MANIFEST，写明生成时刻与**哪些内容被截断**。截断必须留痕：
// 不说的话，分析的人会把"只看到了最后 2000 条日志"当成"这段时间只有这些"。
package diagnostics

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
	"k_cockpit/internal/settings"
)

// 分类。
const (
	CategoryConfig  = "config"
	CategoryRuntime = "runtime"
	CategoryLogs    = "logs"
)

// Category 是一个可导出的分类。
type Category struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	// Sensitive 为 true 表示这个分类里**可能含有敏感内容**，导出时需要二次确认。
	//
	// 配置与日志都算：配置里有地址与账号（密钥已打码），日志里有谁在什么时候
	// 做了什么。运行时状态相对干净。
	Sensitive bool `json:"sensitive"`
}

// Categories 返回全部分类。
func Categories() []Category {
	return []Category{
		{
			Key: CategoryConfig, Label: "配置", Sensitive: true,
			Description: "系统设置与它们的来源（环境变量 / 面板 / 默认值）。" +
				"敏感项（密钥、密码、令牌）只显示「已设置」，不导出明文。",
		},
		{
			Key: CategoryRuntime, Label: "运行时状态", Sensitive: false,
			Description: "节点状态、任务队列、调度器与版本信息。" +
				"不含任何密钥，是排查「为什么不工作」时最先要看的一份。",
		},
		{
			Key: CategoryLogs, Label: "日志", Sensitive: true,
			Description: "最近的审计日志（谁在什么时候做了什么）。" +
				"含操作者与来源 IP，请确认可以外发。",
		},
	}
}

// 上限。**必须有**：一个不限量的诊断包会把内存吃满（它是同步生成的），
// 而"日志有 2GB"这件事在导出之前是看不出来的。
const (
	maxAuditRows  = 2000
	maxBundleSize = 8 << 20 // 8 MiB
)

// Service 生成诊断包。
type Service struct {
	db       *gorm.DB
	settings *settings.Service
	registry *scheduler.Registry
	audit    *audit.Recorder
	now      func() time.Time
	// Version 由装配处注入（面板版本），避免本包依赖构建信息。
	Version string
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, set *settings.Service, registry *scheduler.Registry, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, settings: set, registry: registry, audit: recorder, now: time.Now}
}

// Bundle 是一次生成的诊断包。
type Bundle struct {
	// Data 是 zip 内容。
	Data []byte
	// Filename 建议的文件名（含生成时刻）。
	Filename string
	// Truncated 记录被截断的分类与原因，供界面提示。
	Truncated []string `json:"truncated,omitempty"`
}

// Export 生成诊断包。**同步、不落盘。**
func (s *Service) Export(
	ctx context.Context, keys []string, v Viewer, operatorName, clientIP string,
) (*Bundle, error) {
	wanted := map[string]bool{}
	for _, k := range keys {
		wanted[k] = true
	}
	if len(wanted) == 0 {
		// 默认全选：不选等于"我全都想要"，而逐个勾选是额外负担。
		for _, c := range Categories() {
			wanted[c.Key] = true
		}
	}
	for k := range wanted {
		if !validCategory(k) {
			return nil, api.InvalidParameter("未知的导出分类：" + k)
		}
	}

	at := s.now()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	truncated := []string{}

	writeFile := func(name string, content any) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(content)
	}

	if wanted[CategoryConfig] {
		items, _, err := s.settings.List(ctx)
		if err != nil {
			return nil, err
		}
		// settings.List 已经对敏感项返回空值（R-009）——**不再自己打一遍码**，
		// 见包注释第 2 条。
		if err := writeFile("config/settings.json", items); err != nil {
			return nil, api.Internal()
		}
	}

	if wanted[CategoryRuntime] {
		if err := writeFile("runtime/overview.json", s.runtimeOverview(ctx, at)); err != nil {
			return nil, api.Internal()
		}
		if len(s.registry.All()) > 0 {
			if err := writeFile("runtime/schedulers.json", s.registry.All()); err != nil {
				return nil, api.Internal()
			}
		}
	}

	if wanted[CategoryLogs] {
		logs, cut, err := s.auditLogs(ctx)
		if err != nil {
			return nil, err
		}
		if cut {
			truncated = append(truncated, fmt.Sprintf(
				"日志只包含最近 %d 条（更早的没有导出）", maxAuditRows))
		}
		if err := writeFile("logs/audit.json", logs); err != nil {
			return nil, api.Internal()
		}
	}

	manifest := map[string]any{
		"generated_at": at.Format(time.RFC3339),
		"categories":   keysOf(wanted),
		"truncated":    truncated,
		"note": "敏感配置项（密钥、密码、令牌）不会出现在本包中——" +
			"它们显示为「已设置」而不含明文，因此本包可以直接外发。",
	}
	if err := writeFile("MANIFEST.json", manifest); err != nil {
		return nil, api.Internal()
	}
	if err := zw.Close(); err != nil {
		return nil, api.Internal()
	}
	if buf.Len() > maxBundleSize {
		return nil, api.ValidationFailed(fmt.Sprintf(
			"诊断包超过 %d MB，请减少分类后重试（日志分类通常是最大的一份）", maxBundleSize>>20))
	}

	// **导出必须记审计。**
	//
	// 这个包等于把系统的内部状态带走，而它正是排查"谁做了什么"时最有用的
	// 东西之一。操作者、时刻、来源 IP 与选了哪些分类要留痕。
	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			ResourceType: "diagnostics", Action: "diagnostics.export",
			Params: map[string]any{
				"categories": keysOf(wanted),
				"bytes":      buf.Len(),
				"truncated":  truncated,
			},
			Success: true, ClientIP: clientIP,
		})
	}

	return &Bundle{
		Data:      buf.Bytes(),
		Filename:  "kc-diagnostics-" + at.Format("20060102-150405") + ".zip",
		Truncated: truncated,
	}, nil
}

// runtimeOverview 汇总运行时状态。
//
// 刻意**只取计数与状态，不取具体内容**：诊断包要能安全外发，而"有 12 个
// 待执行任务"与"这 12 个任务分别是什么（可能含虚拟机名、路径）"是两回事。
// 需要细节时用户会去看任务中心——那里有权限控制。
func (s *Service) runtimeOverview(ctx context.Context, at time.Time) map[string]any {
	out := map[string]any{
		"generated_at":    at.Format(time.RFC3339),
		"panel_version":   s.Version,
		"migration_state": "见 config 分类",
	}

	type count struct {
		K string `json:"k"`
		N int64  `json:"n"`
	}
	var nodeTotal, nodeEnrolled, vmTotal, vmRunning int64
	s.db.WithContext(ctx).Model(&model.Node{}).Count(&nodeTotal)
	s.db.WithContext(ctx).Model(&model.Node{}).
		Where("enroll_state = ?", model.NodeEnrollEnrolled).Count(&nodeEnrolled)
	s.db.WithContext(ctx).Model(&model.VM{}).Count(&vmTotal)
	s.db.WithContext(ctx).Model(&model.VM{}).
		Where("status = ?", model.VMStatusRunning).Count(&vmRunning)

	out["nodes"] = []count{{K: "total", N: nodeTotal}, {K: "enrolled", N: nodeEnrolled}}
	out["vms"] = []count{{K: "total", N: vmTotal}, {K: "running", N: vmRunning}}

	// 任务按状态计数：一眼能看出有没有堆积。
	var rows []struct {
		Status string
		N      int64
	}
	if err := s.db.WithContext(ctx).Model(&model.Task{}).
		Select("status, count(*) as n").Group("status").Scan(&rows).Error; err != nil {
		log.Printf("[diagnostics] 统计任务失败: %v", err)
	} else {
		tasks := make([]count, 0, len(rows))
		for _, r := range rows {
			tasks = append(tasks, count{K: r.Status, N: r.N})
		}
		out["tasks"] = tasks
	}
	return out
}

// auditLogs 取最近的审计日志，并报告是否被截断。
func (s *Service) auditLogs(ctx context.Context) ([]model.AuditLog, bool, error) {
	var rows []model.AuditLog
	if err := s.db.WithContext(ctx).
		Order("id DESC").Limit(maxAuditRows + 1).Find(&rows).Error; err != nil {
		log.Printf("[diagnostics] 查询审计日志失败: %v", err)
		return nil, false, api.Internal()
	}
	cut := false
	if len(rows) > maxAuditRows {
		rows = rows[:maxAuditRows]
		cut = true
	}
	return rows, cut, nil
}

func validCategory(k string) bool {
	switch k {
	case CategoryConfig, CategoryRuntime, CategoryLogs:
		return true
	}
	return false
}

func keysOf(m map[string]bool) []string {
	out := []string{}
	for _, c := range Categories() {
		if m[c.Key] {
			out = append(out, c.Key)
		}
	}
	return out
}

// Viewer 是导出所需的最小身份信息。
type Viewer struct {
	UserID   int64
	IsAdmin  bool
	Username string
}
