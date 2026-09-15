package task

import (
	"context"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Viewer 是查询视角。语义与授权层一致，直接复用其定义，避免两处各定义一份
// 然后慢慢分叉。
type Viewer = authz.Viewer

// Filter 是任务查询条件。
type Filter struct {
	Status       string
	Type         string
	ResourceType string
	ResourceID   int64
	Page         int
	PageSize     int
	Viewer       Viewer
}

// List 返回任务列表与总数。
//
// 总数是**过滤后**的数量：返回全局总数会让 tenant 通过翻页差异推断出
// 他人有多少任务（f-1-06 §5.2）。
func (q *Queue) List(ctx context.Context, f Filter) ([]model.Task, int64, error) {
	page, pageSize := normalizePage(f.Page, f.PageSize)

	query := q.db.WithContext(ctx).Model(&model.Task{})
	query = applyFilter(query, f)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		log.Printf("[task] 统计任务失败: %v", err)
		return nil, 0, api.Internal()
	}

	var tasks []model.Task
	if err := query.Order("id DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&tasks).Error; err != nil {
		log.Printf("[task] 查询任务失败: %v", err)
		return nil, 0, api.Internal()
	}
	return tasks, total, nil
}

// Get 返回任务详情；不属于当前视角的任务返回 404。
//
// 用 404 而非 403：后者等于告诉调用方「这个 ID 是存在的」（f-1-06 R-005）。
func (q *Queue) Get(ctx context.Context, id int64, v Viewer) (*model.Task, error) {
	query := q.db.WithContext(ctx).Where("id = ?", id)
	if !v.IsAdmin {
		query = query.Where("owner_id = ?", v.UserID)
	}

	var t model.Task
	err := query.First(&t).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("任务不存在")
	case err != nil:
		log.Printf("[task] 查询任务失败: %v", err)
		return nil, api.Internal()
	}
	return &t, nil
}

// Cancel 请求取消任务（f-7-01 R-007）。
//
// 两种情形处理不同：
//   - `pending`：还没开始执行，直接置 `canceled`；
//   - `running`：置 `cancel_requested` 并等待执行方确认——**不立刻标记为已取消**，
//     因为操作可能已经产生了副作用，谎报取消会让用户以为没有发生任何事。
func (q *Queue) Cancel(
	ctx context.Context, id int64, v Viewer, operatorID int64, operatorName, clientIP string,
) (*model.Task, error) {
	t, err := q.Get(ctx, id, v)
	if err != nil {
		return nil, err
	}

	if t.IsTerminal() {
		return nil, api.Conflict("任务已结束，无法取消")
	}
	if t.Status == model.TaskUnknown {
		return nil, api.Conflict("任务状态未知（节点失联），请等待节点恢复后再处理")
	}

	updates := map[string]any{"cancel_requested": true}
	if t.Status == model.TaskPending {
		now := time.Now()
		updates["status"] = model.TaskCanceled
		updates["finished_at"] = now
	}

	if err := q.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[task] 取消任务失败: %v", err)
		return nil, api.Internal()
	}

	q.record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		ResourceType: deref(t.ResourceType),
		ResourceID:   deref(t.ResourceID),
		ResourceName: deref(t.ResourceName),
		Action:       "task.cancel",
		BeforeState:  map[string]any{"task_id": t.ID, "status": t.Status},
		AfterState:   map[string]any{"cancel_requested": true},
		Success:      true,
		ClientIP:     clientIP,
	})

	return q.Get(ctx, id, v)
}

// Cleanup 清理终态任务（f-7-01 R-016）。
//
// 只删除终态任务：`pending` / `running` / `unknown` 一律不可清理——
// 删除一个执行中的任务会让它的结果永远无处落定。
func (q *Queue) Cleanup(ctx context.Context, before time.Time) (int64, error) {
	res := q.db.WithContext(ctx).
		Where("status IN ? AND finished_at < ?",
			[]string{model.TaskSuccess, model.TaskFailed, model.TaskCanceled}, before).
		Delete(&model.Task{})
	if res.Error != nil {
		log.Printf("[task] 清理任务失败: %v", res.Error)
		return 0, api.Internal()
	}
	return res.RowsAffected, nil
}

func applyFilter(query *gorm.DB, f Filter) *gorm.DB {
	if f.Status != "" {
		query = query.Where("status = ?", f.Status)
	}
	if f.Type != "" {
		query = query.Where("type = ?", f.Type)
	}
	if f.ResourceType != "" && f.ResourceID > 0 {
		query = query.Where("resource_type = ? AND resource_id = ?", f.ResourceType, f.ResourceID)
	}
	// 归属过滤：tenant 只能看到自己名下的任务。
	if !f.Viewer.IsAdmin {
		query = query.Where("owner_id = ?", f.Viewer.UserID)
	}
	return query
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
