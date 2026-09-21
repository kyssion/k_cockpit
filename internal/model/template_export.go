package model

import "time"

// 模板导出状态。
//
// 与虚拟机导出（vm_export）同一套词汇：两者都是"一个打包任务"，状态机
// 不同只会让两处各自的判断规则分叉。
const (
	TemplateExportPending = "pending"
	TemplateExportRunning = "running"
	TemplateExportSuccess = "success"
	TemplateExportFailed  = "failed"
)

// TemplateExport 对应 template_export 表：一次模板导出的产物（F-3-05）。
//
// 为什么不复用 vm_export：那张表的主语是**虚拟机**，而这里的主语是模板。
// 共用一张表就得让"导出的是谁"变成一列可空的外键，而可空外键的代价是
// 每个查询都要多写一句条件——漏写一处就会出现"虚拟机导出列表里混进了
// 模板包"。
type TemplateExport struct {
	ID         int64 `gorm:"primaryKey"`
	NodeID     int64 `gorm:"not null;index:idx_template_export_node"`
	TemplateID int64 `gorm:"not null;index:idx_template_export_template"`
	// TemplateName 在导出时快照：模板随后可能被删除，而那时导出包仍然存在
	// ——如果只留 ID，列表上会显示一行"来自 #7"却说不清是哪个模板。
	TemplateName string `gorm:"size:64;not null"`

	// RelPath 是导出包相对**用户存储根**的路径。
	//
	// 放在用户存储里而不是某个临时目录：这样它能被下载、能被作为导入来源
	// 选中，也受用户存储配额约束——一个几十 GB 的临时包不该躲在配额之外。
	RelPath   string `gorm:"size:512;not null"`
	Filename  string `gorm:"size:255;not null"`
	SizeBytes int64  `gorm:"not null;default:0"`

	Status string  `gorm:"size:16;not null;default:pending"`
	Error  *string `gorm:"size:255"`

	CreatedBy  *int64
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// TableName 固定表名。
func (TemplateExport) TableName() string { return "template_export" }

// IsReady 报告产物是否已可下载。
func (e *TemplateExport) IsReady() bool { return e.Status == TemplateExportSuccess }

// IsActive 报告任务是否仍在途。
func (e *TemplateExport) IsActive() bool {
	return e.Status == TemplateExportPending || e.Status == TemplateExportRunning
}
