package model

import "time"

// 用户存储里的三类资源（F-5-03：按 ISO 镜像 / 文件共享 / 虚拟磁盘三类管理）。
const (
	// FileCategoryISO 安装镜像。它可以被挂载到虚拟机的光驱上。
	FileCategoryISO = "iso"
	// FileCategoryShare 普通文件共享：用户放进去、之后从虚拟机里取的任何文件。
	FileCategoryShare = "share"
	// FileCategoryDisk 虚拟磁盘。它可以作为数据盘挂到虚拟机上。
	FileCategoryDisk = "disk"
)

// StorageFile 对应 storage_file 表：用户存储空间里的一个文件（F-5-03 / F-5-05）。
//
// `rel_path` 是**相对于该用户在节点上的存储根**的路径，而不是宿主机绝对
// 路径。理由与目录共享（f-5-06）完全相同：存绝对路径意味着存储根一旦变更
// （换盘、迁移、重建用户空间），库里所有记录都会指向一个不存在的位置，而
// 那些记录看起来完全正常。存相对路径则天然跟着根走。
type StorageFile struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_storage_file_rel_path,priority:1"`
	// UserID 可为空：为空表示系统预置文件（所有用户可见）。
	UserID *int64 `gorm:"index:idx_storage_file_user_id"`

	RelPath string `gorm:"size:512;not null;uniqueIndex:uniq_storage_file_rel_path,priority:2"`
	// Category 取值 iso / share / disk（见 FileCategoryISO 等）。
	//
	// 它决定这个文件**能被怎么用**，因此不能从扩展名推断：一个 .iso 被
	// 上传成 share 类就不该出现在「挂载镜像」的候选里。类别由用户在上传时
	// 明确选择，而不是系统按后缀猜——猜错的结果是一个看起来可用、实际
	// 挂上去虚拟机起不来的镜像。
	Category string `gorm:"size:16;not null"`
	Filename string `gorm:"size:255;not null"`
	// SizeBytes 是文件大小。配额核算依赖它，因此它必须与实际写入一致
	// ——上传完成时会以节点回报的实际大小为准回写（见上传会话的完成流程）。
	SizeBytes int64 `gorm:"not null;default:0"`

	// SHA256 是内容摘要，用于**秒传**（f-5-04）。
	//
	// **它的查重范围必须限定在同一用户内**：按摘要全局查会让用户 A 通过
	// 「上传 → 秒传成功」推断出用户 B 是否存在某个文件——文件内容本身
	// 不可读，但「它存在」这条信息已经泄漏了。见 Service.FindByDigest。
	SHA256 *string `gorm:"size:64;index:idx_storage_file_sha256"`

	// OsType / OsVariant / MinDiskGB 是识别出的系统信息（f-5-05），仅对
	// ISO 有意义。
	//
	// MinDiskGB 尤其重要：它让「用这个镜像建虚拟机」能提前拦住磁盘过小的
	// 配置，而不是等到安装过程中途失败——那时用户已经等了几分钟，而失败
	// 信息来自安装器，与他刚才填的磁盘大小联系不起来。
	OsType    *string `gorm:"size:64"`
	OsVariant *string `gorm:"size:64"`
	MinDiskGB int     `gorm:"not null;default:0"`

	// UploadedAt 为空的记录表示「已登记但内容尚未写入」。
	//
	// 记录在上传**受理时**就创建（大文件可能传几十分钟），此时它必须
	// 不可见——否则用户会在列表里看到一个大小正常、点开却没有内容的文件。
	UploadedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (StorageFile) TableName() string { return "storage_file" }

// IsReady 报告该文件的内容是否已经就绪（而非仅登记）。
func (f *StorageFile) IsReady() bool { return f.UploadedAt != nil }

// ValidFileCategory 报告类别取值是否合法。
func ValidFileCategory(c string) bool {
	switch c {
	case FileCategoryISO, FileCategoryShare, FileCategoryDisk:
		return true
	}
	return false
}
