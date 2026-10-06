// Package node 是节点域的数据访问层（repository）。
//
// 层规范（见 docs/04-engineering/CODING_STANDARDS.md「分层与 repository」）：
//   - Repo 方法承载**服务需要的查询与写入**，原样下沉、不改语义；
//   - 返回原始数据与错误（如 gorm.ErrRecordNotFound）——翻译成对外的
//     api.Error 是服务层的事，repo 不持有 HTTP 语义；
//   - 不做业务判断：唯一索引冲突原样返回、软删除记录照查不误，「名字已
//     存在」「节点不存在」这些结论属于服务层。
//
// 它是 repository 层的第一个域（范式样本）：其余域按同一形状分批迁入。
package node

import (
	"context"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// Repo 持有数据库连接，提供节点域全部数据访问。
type Repo struct {
	db *gorm.DB
}

// NewRepo 构造节点域数据访问。
func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

// ListNodes 按 id 升序返回全部节点。
func (r *Repo) ListNodes(ctx context.Context) ([]model.Node, error) {
	var nodes []model.Node
	err := r.db.WithContext(ctx).Order("id").Find(&nodes).Error
	return nodes, err
}

// GetNode 按 ID 取节点；不存在时返回 gorm.ErrRecordNotFound。
func (r *Repo) GetNode(ctx context.Context, id int64) (*model.Node, error) {
	var node model.Node
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&node).Error
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// FindByEnrollToken 按注册令牌哈希取**待接入**状态的节点。
func (r *Repo) FindByEnrollToken(ctx context.Context, tokenHash string) (*model.Node, error) {
	var node model.Node
	err := r.db.WithContext(ctx).
		Where("enroll_token_hash = ? AND enroll_state = ?", tokenHash, model.NodeEnrollPending).
		First(&node).Error
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// CreateNode 插入节点记录。唯一索引冲突原样返回，由服务层判定语义。
func (r *Repo) CreateNode(ctx context.Context, node *model.Node) error {
	return r.db.WithContext(ctx).Create(node).Error
}

// UpdateNode 按列更新节点（updates 的键即列名）。
func (r *Repo) UpdateNode(ctx context.Context, id int64, updates map[string]any) error {
	return r.db.WithContext(ctx).Model(&model.Node{}).
		Where("id = ?", id).Updates(updates).Error
}

// CountNodeVMs 统计节点上的虚拟机总数与运行中数。
//
// 口径是控制面投影（present = true），不是节点上真实的域数——「面板显示
// 的数字必须与列表页一致」的判断在服务层，这里只负责按该口径数数。
func (r *Repo) CountNodeVMs(ctx context.Context, nodeID int64) (total, running int64, err error) {
	if err = r.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND present = ?", nodeID, true).Count(&total).Error; err != nil {
		return 0, 0, err
	}
	err = r.db.WithContext(ctx).Model(&model.VM{}).
		Where("node_id = ? AND present = ? AND status = ?", nodeID, true, model.VMStatusRunning).
		Count(&running).Error
	return total, running, err
}

// NodeNames 返回全部节点名（模拟节点预置的幂等判断用）。
func (r *Repo) NodeNames(ctx context.Context) ([]string, error) {
	var names []string
	err := r.db.WithContext(ctx).Model(&model.Node{}).Pluck("name", &names).Error
	return names, err
}
