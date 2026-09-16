package task

import (
	"context"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// Reporter 记录一个任务的阶段流水，并同步 task.current_stage 与 progress。
//
// 它取代了此前那个**定义了却从未被调用**的 ReportProgress：那个函数只接受
// 一个字符串，无法表达「第几步、共几步、何时开始、何时结束」，因此没人愿意
// 接它。现在把落点放在阶段上，进度是阶段推进的**派生值**——顺序天然一致，
// 不会出现「进度 80% 但阶段显示还在校验」这类自相矛盾的界面。
//
// **所有方法都对 nil 接收者安全。** 执行器通过 ReporterFrom(ctx) 取它，而
// 直接调用执行器的地方（大量单元测试）没有队列、也就没有记录器。让每个
// 执行器都写一遍 `if rep != nil` 只会把噪声铺得到处都是，而且迟早有人漏写。
//
// 并发安全：节点的阶段回调与执行器自身的收尾可能来自不同 goroutine。
type Reporter struct {
	db     *gorm.DB
	taskID int64

	mu sync.Mutex
	// seq 是已写入的阶段数，下一个阶段的序号是 seq+1。
	seq int
	// 当前尚未收尾的阶段。
	open *model.TaskStage
	// total 是节点报告的总步数；0 表示节点未给出。
	total int
}

// NewReporter 构造记录器。
func NewReporter(db *gorm.DB, taskID int64) *Reporter {
	return &Reporter{db: db, taskID: taskID}
}

// reporterKey 是 context 中记录器的键。
type reporterKey struct{}

// WithReporter 把记录器放进 context。由队列在派发任务时调用。
func WithReporter(ctx context.Context, r *Reporter) context.Context {
	return context.WithValue(ctx, reporterKey{}, r)
}

// ReporterFrom 取出当前任务的记录器；不存在时返回 nil。
//
// 返回 nil 是正常情况（测试里直接调用执行器），调用方**不需要**判空——
// 所有方法都支持 nil 接收者。
func ReporterFrom(ctx context.Context) *Reporter {
	r, _ := ctx.Value(reporterKey{}).(*Reporter)
	return r
}

// OnStage 返回可直接挂到 agent.Operation.OnStage 上的回调。
//
// 回调把 ctx 换成 context.WithoutCancel：阶段记录是**记账**，它描述的是
// 「节点说它做了这一步」这一已经发生的事实。任务被取消后这些记录依然需要
// 落库，否则时间线会永远缺一个尾巴，而缺失的尾巴看起来像节点没有响应。
func (r *Reporter) OnStage() func(agent.Stage) {
	if r == nil {
		// 返回 nil 而不是一个空函数：mock 与真实客户端都会跳过 nil，
		// 而没有意外行为比多一次调用更省事。
		return nil
	}
	return func(s agent.Stage) {
		r.Begin(context.WithoutCancel(context.Background()), s.Key, s.Name, s.Index, s.Total)
	}
}

// Dispatch 下发一次节点操作，并把这期间发生的事情接到时间线上。
//
// 它做两件事，都必须在**同一处**完成：
//   - 把 op.OnStage 接到本记录器，让节点上报的阶段进入时间线；
//   - 先记一个「下发指令」阶段，再调节点。
//
// 第二条尤其重要，因为它是**指令未送达**时唯一能留下的痕迹。那种情况下
// 节点什么都没做、也就什么都没上报，如果控制面自己不记这一步，时间线会是
// 空的——而「空的」与「不支持展示」看起来是同一件事，用户看不出到底是
// 请求没发出去，还是发出去了但节点没响应。
func (r *Reporter) Dispatch(
	ctx context.Context, client agent.Client, op agent.Operation,
) (*agent.Result, error) {
	if r != nil {
		op.OnStage = r.OnStage()
		r.Mark(ctx, "local.dispatch", "下发指令到节点")
	}
	return client.Execute(ctx, op)
}

// Begin 记录一个阶段开始，并收尾上一个尚未结束的阶段。
//
// 上一个阶段按**成功**收尾：节点只有在新阶段开始时才回报，因此「上一个
// 已经开始、现在轮到了下一个」就意味着上一个已经过去——如果它失败了，
// 整次调用会以失败结束，不会再上报新阶段。
func (r *Reporter) Begin(ctx context.Context, key, name string, index, total int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.closeOpen(ctx, now, model.StageSuccess, "")

	r.seq++
	if index <= 0 {
		index = r.seq
	}
	if total > 0 {
		r.total = total
	}

	stage := model.TaskStage{
		TaskID:    r.taskID,
		Seq:       r.seq,
		Key:       key,
		Name:      name,
		Status:    model.StageRunning,
		StartedAt: &now,
	}
	if err := r.db.WithContext(ctx).Create(&stage).Error; err != nil {
		log.Printf("[task] 记录任务 %d 阶段 %s 失败: %v", r.taskID, key, err)
		// 记录失败不阻断执行：任务本身是否成功不取决于时间线能否写下来。
		// 但 open 仍要置位，否则收尾会去更新一个不存在的行。
	}
	r.open = &stage

	r.syncTask(ctx, key, r.progressAt(index))
}

// Mark 记录一个**由控制面完成**的瞬时阶段（下发指令、回写投影）。
//
// 与节点上报的阶段区分开：控制面自己的步骤用 Local 前缀的 key，界面上就能
// 把「我们做了什么」与「节点做了什么」分开——出问题时第一件要判断的就是
// 「指令到底有没有送到节点」。
func (r *Reporter) Mark(ctx context.Context, key, name string) {
	if r == nil {
		return
	}
	r.Begin(ctx, key, name, 0, 0)
	r.Success(ctx)
}

// Success 把当前阶段收尾为成功，并把任务进度推到 100。
func (r *Reporter) Success(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeOpen(ctx, time.Now(), model.StageSuccess, "")
	r.syncTask(ctx, "", 100)
}

// Fail 把当前阶段收尾为失败，并记下原因。
//
// 失败原因写到**阶段**上而不是只写任务：任务是整体结果，阶段是「卡在哪」。
// 只把原因写在任务上，用户知道失败了却不知道失败在哪一步，还得自己去猜。
func (r *Reporter) Fail(ctx context.Context, message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeOpen(ctx, time.Now(), model.StageFailed, message)
}

// closeOpen 收尾当前阶段。调用方必须已持有锁。
func (r *Reporter) closeOpen(ctx context.Context, now time.Time, status, message string) {
	if r.open == nil {
		return
	}
	updates := map[string]any{"status": status, "finished_at": now}
	if message != "" {
		// 阶段说明截断到列宽：它面向用户，超长内容应当进日志而不是撑爆列。
		if len(message) > 500 {
			message = message[:500]
		}
		updates["message"] = message
	}
	if err := r.db.WithContext(ctx).Model(&model.TaskStage{}).
		Where("id = ?", r.open.ID).Updates(updates).Error; err != nil {
		log.Printf("[task] 收尾任务 %d 阶段 %d 失败: %v", r.taskID, r.open.ID, err)
	}
	r.open = nil
}

// syncTask 同步任务的当前阶段与进度。
//
// 只在任务仍在执行时更新：一个已失败或已取消的任务被后续写入覆盖掉当前阶段，
// 会让界面显示「正在创建磁盘」而状态栏写着「已失败」。
func (r *Reporter) syncTask(ctx context.Context, stageKey string, progress int) {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	updates := map[string]any{
		"progress":         progress,
		"last_reported_at": time.Now(),
	}
	if stageKey != "" {
		updates["current_stage"] = stageKey
	}
	if err := r.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ?", r.taskID, model.TaskRunning).
		Updates(updates).Error; err != nil {
		log.Printf("[task] 同步任务 %d 阶段进度失败: %v", r.taskID, err)
	}
}

// progressAt 把「第 index 步 / 共 total 步」换算为进度。
//
// 换算成 (index-1)/total 而不是 index/total：进度表示的应当是**已经做完的
// 部分**，把刚开始的那一步算进去，会让进度条在第一步就跳到 25%，而那一步
// 其实一步都还没走完。
//
// total 未知（节点没给）时返回 0：界面据此显示为「进行中」而不是一个凭空的
// 百分比。进度条停在 0% 比显示一个编造的数字要好——后者会让人以为真在推进。
func (r *Reporter) progressAt(index int) int {
	if r.total <= 0 || index <= 0 {
		return 0
	}
	return (index - 1) * 100 / r.total
}

// Stages 返回某任务的阶段流水，按序号升序。
//
// 排序用 seq 而不是 started_at：同一毫秒内开始的阶段时间戳完全相同，
// 靠时间排序会得到随机顺序，而顺序错乱的时间线会把人引向错误的结论。
func Stages(ctx context.Context, db *gorm.DB, taskID int64) ([]model.TaskStage, error) {
	var rows []model.TaskStage
	err := db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("seq ASC").
		Find(&rows).Error
	return rows, err
}
