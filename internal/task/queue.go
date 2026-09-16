// Package task 实现异步任务队列。
//
// 它是**所有异步操作的载体**（f-7-01 R-001）：可能超过数秒的操作一律入队，
// 接口立即返回任务标识，执行结果通过任务状态与实时通道反馈。
//
// 职责边界：本包负责调度与**状态流转**，不关心任务具体做什么——执行逻辑由
// 业务模块实现 Executor 并注册。这样新增能力时只需加一个 Executor，队列不动。
package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// Executor 是一种任务类型的执行逻辑。
type Executor interface {
	// Type 返回该执行器处理的任务类型。
	Type() string
	// Run 执行任务。返回 nil 表示成功。
	//
	// 返回的错误会写入任务的 error 字段并展示给用户（f-7-01 R-009），
	// 因此**实现方必须确保其中不含内部细节**（堆栈、SQL、路径）。
	Run(ctx context.Context, t *model.Task) error
}

// Options 是队列的可调参数。
type Options struct {
	// MaxConcurrent 是全局并发上限。
	MaxConcurrent int
	// PollInterval 是调度轮询间隔；入队会立即唤醒，它只是兜底。
	PollInterval time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{MaxConcurrent: 4, PollInterval: 2 * time.Second}
}

// Queue 是任务队列。
type Queue struct {
	db        *gorm.DB
	audit     *audit.Recorder
	opts      Options
	executors map[string]Executor

	wake chan struct{}
	wg   sync.WaitGroup
}

// NewQueue 构造队列。
func NewQueue(db *gorm.DB, recorder *audit.Recorder, opts Options) *Queue {
	def := DefaultOptions()
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = def.MaxConcurrent
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = def.PollInterval
	}
	return &Queue{
		db:        db,
		audit:     recorder,
		opts:      opts,
		executors: make(map[string]Executor),
		wake:      make(chan struct{}, 1),
	}
}

// Register 注册任务执行器。
func (q *Queue) Register(e Executor) {
	q.executors[e.Type()] = e
}

// Start 启动调度循环。
func (q *Queue) Start(ctx context.Context) {
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		ticker := time.NewTicker(q.opts.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-q.wake:
			}
			q.dispatch(ctx)
		}
	}()
}

// Stop 等待在途任务结束。
//
// 它**不取消**正在执行的任务：中断一个「正在创建虚拟机」的操作会把系统
// 留在中间状态，比让它跑完更糟。
func (q *Queue) Stop() {
	q.wg.Wait()
}

// Spec 是入队参数。
type Spec struct {
	Type string
	// NodeID 为 0 表示该任务不针对特定节点。
	NodeID int64
	// ResourceType / ResourceID 构成资源锁键（R-003）；为 0 表示不参与互斥。
	ResourceType string
	ResourceID   int64
	ResourceName string
	OwnerID      int64
	CreatedBy    int64
	// Params 会被序列化为 JSON 存储。**调用方负责脱敏**（R-009）。
	Params any
	// IdempotencyKey 非空时保证同一意图只入队一次（R-005）。
	IdempotencyKey string
}

// Enqueue 入队一个任务。
//
// 幂等键重复时返回**已有任务**而非报错：用户重复点击同一个按钮不是错误，
// 让前端拿到同一个任务标识继续跟踪进度才是它期望的行为。
func (q *Queue) Enqueue(ctx context.Context, spec Spec) (*model.Task, error) {
	if _, ok := q.executors[spec.Type]; !ok {
		// 在入队处就拒绝，而不是等调度时才发现——那时用户已经看到「任务已创建」。
		return nil, api.InvalidParameter("不支持的任务类型: " + spec.Type)
	}

	t := model.Task{
		Type:         spec.Type,
		Status:       model.TaskPending,
		NodeID:       optID(spec.NodeID),
		ResourceType: optStr(spec.ResourceType),
		ResourceID:   optID(spec.ResourceID),
		ResourceName: optStr(spec.ResourceName),
		OwnerID:      optID(spec.OwnerID),
		CreatedBy:    optID(spec.CreatedBy),
		Params:       toJSON(spec.Params),
	}
	if spec.IdempotencyKey != "" {
		t.IdempotencyKey = &spec.IdempotencyKey
	}

	if err := q.db.WithContext(ctx).Create(&t).Error; err != nil {
		if spec.IdempotencyKey != "" && isDuplicateKey(err) {
			existing, findErr := q.findByIdempotencyKey(ctx, spec.IdempotencyKey)
			if findErr == nil {
				return existing, nil
			}
		}
		log.Printf("[task] 入队失败: %v", err)
		return nil, api.Internal()
	}

	q.record(ctx, audit.Entry{
		OperatorID:   spec.CreatedBy,
		ResourceType: spec.ResourceType,
		ResourceID:   spec.ResourceID,
		ResourceName: spec.ResourceName,
		Action:       "task.enqueue",
		AfterState:   map[string]any{"task_id": t.ID, "type": t.Type, "resource_id": spec.ResourceID},
		Success:      true,
	})

	q.signal()
	return &t, nil
}

// dispatch 扫描待执行任务并按并发上限与资源互斥规则派发。
func (q *Queue) dispatch(ctx context.Context) {
	var running int64
	if err := q.db.WithContext(ctx).Model(&model.Task{}).
		Where("status = ?", model.TaskRunning).Count(&running).Error; err != nil {
		log.Printf("[task] 统计运行中任务失败: %v", err)
		return
	}
	if running >= int64(q.opts.MaxConcurrent) {
		return
	}

	var pending []model.Task
	if err := q.db.WithContext(ctx).
		Where("status = ?", model.TaskPending).
		Order("id").
		Limit(q.opts.MaxConcurrent).Find(&pending).Error; err != nil {
		log.Printf("[task] 查询待执行任务失败: %v", err)
		return
	}

	// locked 记录本轮已被占用的资源，避免同一资源在一个调度周期内被并行派发。
	locked := map[string]bool{}
	for i := range pending {
		if running >= int64(q.opts.MaxConcurrent) {
			return
		}

		t := &pending[i]
		if key := lockKey(t); key != "" {
			if locked[key] || q.hasRunningOn(ctx, t) {
				continue // 资源忙：保持 pending，等锁释放（R-003）
			}
			locked[key] = true
		}

		if q.claim(ctx, t) {
			running++
			q.wg.Add(1)
			go q.run(ctx, t)
		}
	}
}

// claim 用乐观更新把任务从 pending 抢到 running。
//
// 必须带 status 条件：否则多个调度周期（或未来多实例）会重复执行同一任务。
// 这是一个「执行一次」的保证，比任何标志位都可靠——它由数据库的原子更新兜底。
func (q *Queue) claim(ctx context.Context, t *model.Task) bool {
	now := time.Now()
	res := q.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ?", t.ID, model.TaskPending).
		Updates(map[string]any{
			"status":        model.TaskRunning,
			"started_at":    now,
			"dispatched_at": now,
		})
	if res.Error != nil {
		log.Printf("[task] 抢占任务 %d 失败: %v", t.ID, res.Error)
		return false
	}
	return res.RowsAffected == 1
}

// run 执行任务并落定状态。
func (q *Queue) run(ctx context.Context, t *model.Task) {
	defer q.wg.Done()

	exec, ok := q.executors[t.Type]
	if !ok {
		q.finish(ctx, t, model.TaskFailed, "未注册的任务类型: "+t.Type)
		return
	}

	// 阶段记录器由**队列**创建并放进 context，而不是每个执行器各自构造：
	//
	//   - 终态的收尾只在这里发生一次。若让执行器自己收尾，12 个执行器里
	//     但凡有一个在错误分支上忘了调，就会留下一个永远停在「执行中」的
	//     阶段——而那种记录比没有记录更让人困惑；
	//   - 执行器直接调用（大量单元测试）时没有队列，也就没有记录器，
	//     此时它的所有方法都是空操作（见 Reporter 的 nil 安全性）。
	reporter := NewReporter(q.db, t.ID)
	ctx = WithReporter(ctx, reporter)

	err := exec.Run(ctx, t)
	if err != nil {
		// 失败原因同时落到阶段上：任务是整体结果，阶段才回答「卡在哪」。
		reporter.Fail(ctx, publicMessage(err))
		q.finish(ctx, t, model.TaskFailed, publicMessage(err))
		return
	}

	// 执行期间可能收到了取消请求（R-007）：以取消为准，避免显示成成功
	// 而用户以为自己阻止了它。
	if q.cancelRequested(ctx, t.ID) {
		reporter.Fail(ctx, "任务已被取消")
		q.finish(ctx, t, model.TaskCanceled, "")
		return
	}

	reporter.Success(ctx)
	q.finish(ctx, t, model.TaskSuccess, "")
}

// finish 落定终态。
func (q *Queue) finish(ctx context.Context, t *model.Task, status, message string) {
	now := time.Now()
	updates := map[string]any{
		"status":           status,
		"finished_at":      now,
		"last_reported_at": now,
	}
	switch status {
	case model.TaskSuccess:
		updates["progress"] = 100
		updates["error"] = nil
	case model.TaskCanceled:
		updates["error"] = nil
	default:
		updates["error"] = message
	}

	if err := q.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ?", t.ID).Updates(updates).Error; err != nil {
		log.Printf("[task] 更新任务 %d 状态失败: %v", t.ID, err)
	}

	q.record(ctx, audit.Entry{
		OperatorID:   deref(t.CreatedBy),
		ResourceType: deref(t.ResourceType),
		ResourceID:   deref(t.ResourceID),
		ResourceName: deref(t.ResourceName),
		Action:       "task.finish",
		AfterState:   map[string]any{"task_id": t.ID, "type": t.Type, "status": status},
		Success:      status == model.TaskSuccess,
		Error:        message,
	})
}

// Stages 返回任务的阶段流水。
//
// **不做归属校验**：调用方必须先用 Get 拿到任务（那一步才做校验）。把两件事
// 分开是因为阶段查询会被高频轮询，而归属校验需要额外一次任务查询——让它在
// 每次轮询里重复一遍没有意义。
func (q *Queue) Stages(ctx context.Context, taskID int64) ([]model.TaskStage, error) {
	return Stages(ctx, q.db, taskID)
}

// 进度与当前阶段的上报已并入 Reporter（见 stage.go）。
//
// 此前这里有一个 ReportProgress，只接受一个字符串，**从未被任何执行器
// 调用**。它的问题是信息量太少：一个孤立的进度数字无法回答「卡在哪一步」，
// 而执行器为了调用它还得自己维护「现在到哪一步了」的计数器——那本该是
// 时间线的职责。

func (q *Queue) hasRunningOn(ctx context.Context, t *model.Task) bool {
	resourceType, resourceID := t.TaskResource()
	if resourceType == "" {
		return false
	}
	var count int64
	err := q.db.WithContext(ctx).Model(&model.Task{}).
		Where("resource_type = ? AND resource_id = ? AND status IN ?",
			resourceType, resourceID, []string{model.TaskRunning, model.TaskUnknown}).
		Count(&count).Error
	if err != nil {
		// 查不到时按「资源忙」处理：宁可让任务多等一轮，
		// 也不要在无法确认的情况下并发操作同一资源。
		log.Printf("[task] 检查资源占用失败: %v", err)
		return true
	}
	return count > 0
}

func (q *Queue) findByIdempotencyKey(ctx context.Context, key string) (*model.Task, error) {
	var t model.Task
	if err := q.db.WithContext(ctx).Where("idempotency_key = ?", key).First(&t).Error; err != nil {
		return nil, err
	}
	return &t, nil
}

func (q *Queue) cancelRequested(ctx context.Context, taskID int64) bool {
	var t model.Task
	if err := q.db.WithContext(ctx).Select("cancel_requested").
		Where("id = ?", taskID).First(&t).Error; err != nil {
		return false
	}
	return t.CancelRequested
}

// signal 唤醒调度循环。
func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default: // 已有待处理的唤醒信号，无需重复
	}
}

func (q *Queue) record(ctx context.Context, e audit.Entry) {
	if q.audit != nil {
		q.audit.Record(ctx, e)
	}
}

// publicMessage 把执行错误转成可展示的文案。
func publicMessage(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	// 非业务错误不把原文交给用户：那可能包含 SQL、路径或堆栈（R-009）。
	return "任务执行失败，请查看服务端日志"
}

// lockKey 返回资源锁键；无资源关联时返回空串。
func lockKey(t *model.Task) string {
	resourceType, resourceID := t.TaskResource()
	if resourceType == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", resourceType, resourceID)
}

// isDuplicateKey 判断错误是否为唯一约束冲突。
func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

func optID(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func toJSON(v any) *string {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		placeholder := `{"_error":"内容无法序列化"}`
		return &placeholder
	}
	s := string(b)
	return &s
}
