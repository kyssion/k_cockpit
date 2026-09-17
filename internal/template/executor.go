package template

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// prepareParams 是 template.prepare 任务的参数。
type prepareParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	Name   string `json:"name"`
	NodeID int64  `json:"node_id"`

	OSType    string `json:"os_type"`
	OSVariant string `json:"os_variant"`
	Remark    string `json:"remark"`
	Published bool   `json:"published"`

	DefaultCPU      int `json:"default_cpu"`
	DefaultMemoryMB int `json:"default_memory_mb"`
	DiskSizeGB      int `json:"disk_size_gb"`
}

// PrepareExecutor 执行 template.prepare 任务。
type PrepareExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPrepareExecutor 构造模板制备执行器。
func NewPrepareExecutor(db *gorm.DB, client agent.Client) *PrepareExecutor {
	return &PrepareExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PrepareExecutor) Type() string { return model.TaskTemplatePrepare }

// Run 下发制备指令并写入模板记录。
//
// 顺序：**先让节点复制成功、再写记录**。反过来的话，复制失败时会留下一条
// 指向不存在磁盘的模板，用户拿它去建机，然后在某个说不清的时刻失败。
func (e *PrepareExecutor) Run(ctx context.Context, t *model.Task) error {
	var p prepareParams
	if !decode(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpTemplatePrepare,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			"name":      p.Name,
			"disk_size": p.DiskSizeGB,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，模板制备指令未送达")
	}
	if !result.Success {
		// 节点侧的失败原因更准（它看得到磁盘空间、源盘状态），原样带出。
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.TemplateDataKey].(agent.TemplateInfo)

	tpl := model.Template{
		NodeID:          p.NodeID,
		Name:            p.Name,
		Status:          model.TemplateReady,
		DiskFormat:      orDefault(info.Format, "qcow2"),
		DiskSizeGB:      pick(info.SizeGB, p.DiskSizeGB),
		MinDiskGB:       p.DiskSizeGB,
		DefaultCPU:      p.DefaultCPU,
		DefaultMemoryMB: p.DefaultMemoryMB,
		Visibility:      model.TemplatePrivate,
		CloneEnabled:    true,
		Published:       p.Published,
		CreatedBy:       t.CreatedBy,
	}
	if info.DiskPath != "" {
		tpl.DiskPath = &info.DiskPath
	}
	if p.OSType != "" {
		tpl.OSType = &p.OSType
	}
	if p.OSVariant != "" {
		tpl.OSVariant = &p.OSVariant
	}
	if p.Remark != "" {
		tpl.Remark = &p.Remark
	}

	if err := e.db.WithContext(ctx).Create(&tpl).Error; err != nil {
		if isDuplicateKey(err) {
			return api.Conflict("同名模板已存在")
		}
		log.Printf("[template] 写入模板记录失败: %v", err)
		return api.Internal()
	}

	log.Printf("[template] 已制备模板 id=%d name=%s node=%d task=%d",
		tpl.ID, tpl.Name, tpl.NodeID, t.ID)
	return nil
}

// deleteParams 是 template.delete 任务的参数。
type deleteParams struct {
	TemplateID int64  `json:"template_id"`
	Name       string `json:"name"`
	DiskPath   string `json:"disk_path"`
}

// DeleteExecutor 执行 template.delete 任务。
type DeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDeleteExecutor 构造模板删除执行器。
func NewDeleteExecutor(db *gorm.DB, client agent.Client) *DeleteExecutor {
	return &DeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DeleteExecutor) Type() string { return model.TaskTemplateDelete }

// Run 下发删除指令并软删记录。
func (e *DeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	var p deleteParams
	if !decode(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpTemplateDelete,
		NodeID: *t.NodeID,
		Target: p.Name,
		Params: map[string]any{"disk_path": p.DiskPath},
	})
	if err != nil {
		return api.Unavailable("节点不可达，删除指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 软删除：克隆出去的虚拟机记着 template_id，物理删除会让这个引用悬空，
	// 事后追查「这台机器的模板是什么」就无从谈起。
	now := time.Now()
	if err := e.db.WithContext(ctx).Model(&model.Template{}).
		Where("id = ?", p.TemplateID).
		Updates(map[string]any{"deleted_at": now}).Error; err != nil {
		log.Printf("[template] 标记模板已删除失败 id=%d: %v", p.TemplateID, err)
	}

	log.Printf("[template] 已删除模板 id=%d name=%s task=%d", p.TemplateID, p.Name, t.ID)
	return nil
}

// isDuplicateKey 判断错误是否为唯一约束冲突。
//
// 同时看 GORM 的归一化错误与驱动的原始文本：前者在部分驱动/版本上不会被
// 填充，只认它会让冲突漏判并退化成一句「服务内部错误」。
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

// decode 解析任务参数。
func decode(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[template] 解析任务参数失败 task=%d: %v", t.ID, err)
		return false
	}
	return true
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// pick 取第一个非零值：节点没报实际大小时回落到配置值。
func pick(actual, fallback int) int {
	if actual > 0 {
		return actual
	}
	return fallback
}
