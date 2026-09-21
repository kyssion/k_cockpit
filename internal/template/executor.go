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

	// 默认硬件：取自源虚拟机，克隆时作为默认值下发。
	DefaultDiskBus     string `json:"default_disk_bus"`
	DefaultNicModel    string `json:"default_nic_model"`
	DefaultMachineType string `json:"default_machine_type"`
	DefaultFirmware    string `json:"default_firmware"`
	DefaultDisplay     string `json:"default_display"`

	// 族与版本（F-3-04）：仅派生制备时有值。
	ParentID *int64 `json:"parent_id"`
	FamilyID *int64 `json:"family_id"`
	Version  int    `json:"version"`
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
	if p.DefaultDiskBus != "" {
		tpl.DefaultDiskBus = &p.DefaultDiskBus
	}
	if p.DefaultNicModel != "" {
		tpl.DefaultNicModel = &p.DefaultNicModel
	}
	if p.DefaultMachineType != "" {
		tpl.DefaultMachineType = &p.DefaultMachineType
	}
	if p.DefaultFirmware != "" {
		tpl.DefaultFirmware = &p.DefaultFirmware
	}
	if p.DefaultDisplay != "" {
		tpl.DefaultVideoModel = &p.DefaultDisplay
	}
	// 族与版本：只有派生制备才写。独立制备的模板 family_id 留空——它的族
	// 会在第一次派生时才成形（那时把自己当成根）。
	if p.ParentID != nil && *p.ParentID > 0 {
		tpl.ParentID = p.ParentID
	}
	if p.FamilyID != nil && *p.FamilyID > 0 {
		tpl.FamilyID = p.FamilyID
	}
	if p.Version > 1 {
		tpl.Version = p.Version
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
	// TemplateIDs / DiskPaths 在**级联删除**时带上整条派生链。
	//
	// 列表而不是单值：级联的本质是"这一批一起没了"，让执行器自己再去查
	// 一遍子树等于把已经算好的结果重算一次，而两处算法不一致时会出现
	// "删了磁盘但记录还在"这类半吊子状态。
	TemplateIDs []int64  `json:"template_ids"`
	DiskPaths   []string `json:"disk_paths"`
	Strategy    string   `json:"strategy"`
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

	// 级联时一次下发全部路径：逐个下发会让"删到一半失败"留下一个既删了
	// 一部分、又查不到另一部分的中间状态。
	paths := p.DiskPaths
	if len(paths) == 0 {
		paths = []string{p.DiskPath}
	}
	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpTemplateDelete,
		NodeID: *t.NodeID,
		Target: p.Name,
		Params: map[string]any{"disk_path": p.DiskPath, "disk_paths": paths},
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
	ids := p.TemplateIDs
	if len(ids) == 0 {
		ids = []int64{p.TemplateID}
	}
	if err := e.db.WithContext(ctx).Model(&model.Template{}).
		Where("id IN ?", ids).
		Updates(map[string]any{"deleted_at": now}).Error; err != nil {
		log.Printf("[template] 标记模板已删除失败 ids=%v: %v", ids, err)
	}

	log.Printf("[template] 已删除模板 ids=%v name=%s strategy=%s task=%d",
		ids, p.Name, p.Strategy, t.ID)
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
