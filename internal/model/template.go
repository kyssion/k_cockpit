package model

import (
	"time"

	"gorm.io/gorm"
)

// 模板的制备状态。
//
// 与 task.status 用同一套词汇（pending/running/success/failed），避免界面上
// 出现两种说法指着同一件事。
const (
	// TemplatePreparing 制备中：磁盘正在复制或转换，此时不可用于创建虚拟机。
	TemplatePreparing = "preparing"
	// TemplateReady 可用。
	TemplateReady = "ready"
	// TemplateFailed 制备失败，磁盘副本可能是不完整的。
	TemplateFailed = "failed"
)

// 模板可见性。
const (
	// TemplatePrivate 仅创建者与自己所属的租户可用。
	TemplatePrivate = "private"
	// TemplatePublic 所有租户可用。
	TemplatePublic = "public"
)

// 克隆方式（f-3-02）。
const (
	// CloneFull 完整克隆：复制整块磁盘。慢、占空间，但克隆体与模板**完全
	// 独立**——模板被删、被改都不会影响已克隆出去的虚拟机。
	CloneFull = "full"
	// CloneLinked 链式克隆（overlay）：新磁盘只记录与模板的差异，秒级完成、
	// 几乎不占空间。代价是**依赖链**：父磁盘缺失或被改，克隆体的数据就
	// 不可用了。因此界面上必须显示「依赖链」并在链被破坏时明确报错。
	CloneLinked = "linked"
)

// Template 对应 template 表：可复用的虚拟机模板（F-3-01 / F-3-02）。
//
// 「模板」在本项目里就是**一块准备好并可写的系统盘**，外加一组默认硬件参数。
// 没有更复杂的东西：创建虚拟机时要么复制它（full），要么以它为 backing file
// 建一块 overlay（linked）。
type Template struct {
	ID int64 `gorm:"primaryKey"`
	// NodeID 模板是**节点内**的资源：磁盘文件就在那个节点的存储池里，
	// 跨节点使用需要先导出再导入。
	NodeID int64  `gorm:"not null;uniqueIndex:uniq_template_node_name,priority:1"`
	Name   string `gorm:"size:64;not null;uniqueIndex:uniq_template_node_name,priority:2"`

	// ParentID 指向制备它的来源模板，用于链式克隆。
	//
	// 保留这条链是为了两件事：显示依赖关系（用户要知道自己的模板是从哪来的），
	// 以及在父模板被删时**拒绝**而不是留下一条静默失效的链。
	ParentID *int64 `gorm:"index:idx_template_parent_id"`
	Version  int    `gorm:"not null;default:1"`

	Status string `gorm:"size:16;not null;default:preparing"`

	StoragePoolID *int64
	// DiskPath 是模板盘在宿主机上的路径。**不暴露给普通用户**：它是内部
	// 实现细节，暴露出去会诱使用户去宿主机上直接操作这个文件。
	DiskPath   *string `gorm:"size:512"`
	DiskFormat string  `gorm:"size:16;not null;default:qcow2"`
	DiskSizeGB int     `gorm:"not null;default:0"`

	// OS 信息供界面展示与筛选；也为「重装系统」（f-2-11）提供判断依据
	// ——只有同 os_type 的模板才适合重装到一台已有数据的机器上。
	OSType    *string `gorm:"size:64"`
	OSVariant *string `gorm:"size:64"`

	// MinDiskGB 是模板自身占用的最小磁盘；用它创建虚拟机时不能小于它。
	MinDiskGB int `gorm:"not null;default:0"`

	DefaultCPU      int     `gorm:"not null;default:0"`
	DefaultMemoryMB int     `gorm:"not null;default:0"`
	DefaultSpec     *string `gorm:"type:text"`

	// Published 表示已发布给其他用户使用。
	Published bool `gorm:"not null;default:false;index:idx_template_node_published,priority:2"`
	// Visibility 取值 private / public。
	Visibility string `gorm:"size:16;not null;default:private"`
	// CloneEnabled 为 false 时不允许再克隆（准备下线但仍要保留已有克隆体）。
	//
	// ⚠️ **GORM 陷阱**（同 model/schedule.go 的 Enabled）：字段带 `default:true`
	// 时，值为 `false` 会被当作零值**省略不写**，数据库随即填入 `true`——
	// 也就是说 `Create(&Template{CloneEnabled: false})` 存进去的是一条
	// **允许克隆**的记录。
	//
	// 生产代码里创建时总是 true（见 template.PrepareExecutor），不受影响；
	// 关闭克隆走 Update（显式列名），那条路径是正确的。改动这里之前请先确认这一点。
	CloneEnabled bool `gorm:"not null;default:true"`
	// Immutable 为 true 时模板盘以只读方式共享，链式克隆更安全但无法更新。
	Immutable bool `gorm:"not null;default:false"`

	PrepareMode *string `gorm:"size:16"`
	Error       *string `gorm:"size:255"`

	// CreatedBy 是模板的创建者。私有模板的可见性以它为准。
	CreatedBy *int64
	Remark    *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
	// 软删除：已被克隆走的模板必须保留记录，否则克隆体的来源就查不到了。
	DeletedAt gorm.DeletedAt
}

// TableName 固定表名。
func (Template) TableName() string { return "template" }

// IsReady 报告模板是否可用于创建虚拟机。
//
// 制备中与失败都不可用，但两者的处理完全不同：前者要等，后者要删掉重建。
// 界面上必须分开显示——把「还在准备」说成「不可用」会让用户白等，
// 把「失败」说成「准备中」会让他一直等下去。
func (t *Template) IsReady() bool { return t.Status == TemplateReady }

// CanClone 报告模板当前是否允许克隆。
func (t *Template) CanClone() bool { return t.IsReady() && t.CloneEnabled }

// DiskPathOf 返回模板盘路径；未设置时返回空串。
func (t *Template) DiskPathOf() string {
	if t.DiskPath == nil {
		return ""
	}
	return *t.DiskPath
}
