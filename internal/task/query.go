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
// 两条约束，每一条对着一类会出事的情况：
//
//  1. **只删终态任务**：`pending` / `running` / `unknown` 一律不可清理——
//     删除一个执行中的任务会让它的结果永远无处落定。
//
//  2. **被引用的任务不删**。有两张表指回任务，而那些引用是**当前状态的一部分**，
//     不是历史：
//
//     vm_schedule.last_task_id    「这条定时任务上次跑的结果」——清掉它，
//     定时任务页上的「上次执行」就永远显示不出来
//     network_capture.task_id     从抓包记录跳到任务详情
//
//     清掉它们指向的任务之后，那些引用会悬空：界面上点过去是一个不存在的
//     任务，而用户看不出是「任务被清理了」还是「记录坏了」。
//
//     换句话说：**清理历史时，不能把当前状态里还指着的那些一起清掉。**
func (q *Queue) Cleanup(ctx context.Context, before time.Time) (int64, error) {
	// **这两张表由别的包拥有**，而清理需要读它们来确定"哪些任务还被指着"。
	// 这个耦合是真实的而不是偶然：清理必须知道当前状态里还有谁引用着任务，
	// 而那份信息在别处。因此测试库也必须建它们——少了它们，清理会以
	// 「服务内部错误」失败，而那个错误与真正的原因（表不存在）看不出关系。
	res := q.db.WithContext(ctx).
		Where("status IN ? AND finished_at < ?",
			[]string{model.TaskSuccess, model.TaskFailed, model.TaskCanceled}, before).
		Where("id NOT IN (SELECT last_task_id FROM vm_schedule WHERE last_task_id IS NOT NULL)").
		Where("id NOT IN (SELECT task_id FROM network_capture WHERE task_id IS NOT NULL)").
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
