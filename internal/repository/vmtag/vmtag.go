// Package vmtag 是虚拟机标签域的数据访问层（ADR-0012）。
//
// 与 repository/node 同一形状：只承载查询与写入，返回原始模型与原始错误；
// 「租户可见范围」由 service 层折算成一个可选的 ownerID 传进来，repo 自身
// 不依赖 authz，也不把 GORM 错误翻译成 api.Error。
package vmtag

import (
	"context"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// Repo 持有数据库连接。
type Repo struct {
	db *gorm.DB
}

// NewRepo 构造标签域数据访问。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// TagCount 是一个标签及其使用次数（Scan 的目标类型）。
type TagCount struct {
	Tag   string `json:"tag" gorm:"column:tag"`
	Count int    `json:"count" gorm:"column:count"`
}

// TagsOfVMs 批量读取多台虚拟机的标签行，按 (vm_id, tag) 排序。
func (r *Repo) TagsOfVMs(ctx context.Context, vmIDs []int64) ([]model.VMTag, error) {
	var rows []model.VMTag
	err := r.db.WithContext(ctx).
		Where("vm_id IN ?", vmIDs).
		Order("vm_id, tag").Find(&rows).Error
	return rows, err
}

// TagsOfVM 返回单台虚拟机的标签（已按字典序）。
func (r *Repo) TagsOfVM(ctx context.Context, vmID int64) ([]string, error) {
	var tags []string
	err := r.db.WithContext(ctx).Model(&model.VMTag{}).
		Where("vm_id = ?", vmID).Order("tag ASC").Pluck("tag", &tags).Error
	return tags, err
}

// ReplaceTags 在一个事务里整体替换标签；事务不能把 *gorm.DB 泄漏回 service。
func (r *Repo) ReplaceTags(ctx context.Context, vmID int64, tags []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("vm_id = ?", vmID).Delete(&model.VMTag{}).Error; err != nil {
			return err
		}
		if len(tags) == 0 {
			return nil
		}
		rows := make([]model.VMTag, 0, len(tags))
		for _, t := range tags {
			rows = append(rows, model.VMTag{VMID: vmID, Tag: t})
		}
		return tx.Create(&rows).Error
	})
}

// VMIDsByTag 返回带该标签的虚拟机 ID。ownerID 非 nil 时只取该用户名下的机器。
func (r *Repo) VMIDsByTag(ctx context.Context, tag string, ownerID *int64) ([]int64, error) {
	query := r.db.WithContext(ctx).Model(&model.VMTag{}).Where("tag = ?", tag)
	if ownerID != nil {
		query = query.Where("vm_id IN (?)", visibleVMs(r.db.WithContext(ctx), *ownerID))
	}
	var ids []int64
	err := query.Pluck("vm_id", &ids).Error
	return ids, err
}

// TagCounts 汇总标签使用次数，ownerID 非 nil 时限定可见范围。
func (r *Repo) TagCounts(ctx context.Context, ownerID *int64) ([]TagCount, error) {
	query := r.db.WithContext(ctx).Model(&model.VMTag{}).
		Select("tag, count(*) AS count").Group("tag").Order("count DESC, tag ASC")
	if ownerID != nil {
		query = query.Where("vm_id IN (?)", visibleVMs(r.db.WithContext(ctx), *ownerID))
	}
	var rows []TagCount
	err := query.Scan(&rows).Error
	return rows, err
}

// GetVM 按 ID 取虚拟机；不存在时返回 gorm.ErrRecordNotFound。
func (r *Repo) GetVM(ctx context.Context, id int64) (*model.VM, error) {
	var vm model.VM
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&vm).Error; err != nil {
		return nil, err
	}
	return &vm, nil
}

func visibleVMs(db *gorm.DB, ownerID int64) *gorm.DB {
	return db.Model(&model.VM{}).Select("id").Where("owner_id = ?", ownerID)
}
