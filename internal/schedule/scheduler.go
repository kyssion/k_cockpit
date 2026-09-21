// Package schedule 执行虚拟机的定时任务（F-7-05）。
//
// 它的职责很窄：**到点了替用户点一次按钮**。三种动作（开机 / 关机 / 删除）
// 都复用已有的任务类型（vm.power / vm.delete）入队，因此不需要任何新的
// agent 能力，也不需要新的执行器。
package schedule

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
	"k_cockpit/internal/task"
	vmsvc "k_cockpit/internal/vm"
)

// Options 是调度器的运行参数。
type Options struct {
	// Interval 是扫描间隔。定时任务的精度以分钟计，扫描频率不需要更高；
	// 太密只会让数据库一直有查询在跑。
	Interval time.Duration
	// Batch 限制一次扫描最多处理多少条。服务长时间停机后可能积压大量
	// 到点记录，一次全处理会把任务队列瞬间打满，反而让正常操作排在后面。
	Batch int
	// MissedThreshold 是「错过多久就不再执行」的阈值。
	MissedThreshold time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		Interval: 30 * time.Second,
		Batch:    50,
		// 一小时：足够覆盖一次普通的服务重启，又不至于让「早上 6 点关机」
		// 在当天下午三点被追着执行。
		MissedThreshold: time.Hour,
	}
}

// Scheduler 周期性扫描到点的定时任务并入队。
type Scheduler struct {
	db    *gorm.DB
	queue *task.Queue
	opts  Options

	stop chan struct{}
	done chan struct{}

	// obs 是调度事件的记录器；为 nil 时不做任何记录。
	obs *scheduler.Recorder

	// snapshots 是「创建快照」动作的入口。它与开机、关机、删除不同：那三种
	// 只是入队，而快照要先建记录、过配额、探测运行态才能入队，因此复用
	// vm 服务的同一条路径（传进来的就是这个服务）。
	snapshots SnapshotCreator
}

// New 构造调度器。
func New(db *gorm.DB, queue *task.Queue, opts Options) *Scheduler {
	def := DefaultOptions()
	if opts.Interval <= 0 {
		opts.Interval = def.Interval
	}
	if opts.Batch <= 0 {
		opts.Batch = def.Batch
	}
	if opts.MissedThreshold <= 0 {
		opts.MissedThreshold = def.MissedThreshold
	}
	return &Scheduler{
		db:    db,
		queue: queue,
		opts:  opts,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// Observe 接入调度事件记录。为 nil 时不做任何记录。
func (s *Scheduler) Observe(rec *scheduler.Recorder) { s.obs = rec }

// Start 启动后台扫描。立即扫一次，之后按 Interval 周期执行。
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		defer close(s.done)

		s.tick(ctx)

		ticker := time.NewTicker(s.opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
}

// Stop 停止后台扫描并等待退出。
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}

// tick 处理一轮到点的定时任务。
func (s *Scheduler) tick(ctx context.Context) {
	var due []model.VMSchedule
	err := s.db.WithContext(ctx).
		Where("enabled = ? AND next_run_at IS NOT NULL AND next_run_at <= ?",
			true, time.Now()).
		Order("next_run_at").
		Limit(s.opts.Batch).
		Find(&due).Error
	if err != nil {
		log.Printf("[schedule] 扫描到点任务失败: %v", err)
		return
	}

	for _, sch := range due {
		s.fire(ctx, sch)
	}
}

// fire 执行一条到点的定时任务。
func (s *Scheduler) fire(ctx context.Context, sch model.VMSchedule) {
	now := time.Now()
	if sch.NextRunAt == nil {
		return
	}
	firedFor := *sch.NextRunAt

	// 先算出下一次时间并**用乐观锁占位**，再入队。
	//
	// 顺序不能反：先入队再更新的话，两个 tick（或多实例）会读到同一个未更新
	// 的 next_run_at，把同一次定时执行成两次——而「开机」执行两次通常没事，
	// 「删除」执行两次就是灾难。先占位，则只有一个能拿到这一轮。
	next := NextAfter(&sch, now)
	res := s.db.WithContext(ctx).Model(&model.VMSchedule{}).
		Where("id = ? AND next_run_at = ?", sch.ID, firedFor).
		Updates(map[string]any{
			"next_run_at": next,
			"last_run_at": now,
		})
	if res.Error != nil {
		log.Printf("[schedule] 更新下次执行时间失败 id=%d: %v", sch.ID, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		// 被另一个 tick 抢走了，正常现象，不必记日志。
		return
	}

	// 从这里往下都是**我们这一轮真的要做的事**，因此取一次名字。
	// 放在占位成功之后：被抢走的那一轮不该产生任何查询。
	name := s.vmNameOf(ctx, sch.VMID)

	// 落后太多说明服务停过机。**刻意不补执行**：
	//
	// 补执行意味着恢复后连着做几次本该分散在不同时间的操作。对「每天 3 点
	// 关机」这类无害；但对「2 点开机、3 点关机」，补执行会让虚拟机刚开机
	// 就被关掉——用户看到的是「它自己开了又关」，而原因埋在几天前的停机里。
	//
	// 宁可漏一次并在界面上留痕，也不要做出用户没预料到的连续操作。
	if now.Sub(firedFor) > s.opts.MissedThreshold {
		s.mark(ctx, sch.ID, "skipped", nil)
		log.Printf("[schedule] 跳过错过的执行 id=%d vm=%d 计划=%s 落后=%s",
			sch.ID, sch.VMID, firedFor.Format(time.RFC3339), now.Sub(firedFor).Round(time.Minute))
		// **跳过也要记。** 用户设了「每天 3 点关机」而某天没关，他唯一能
		// 查到的解释就在这条上——「服务停过机，那次被跳过了」。不记的话，
		// 事件列表里什么都没有，而这与「定时任务根本没配置成功」在界面上
		// 长得一模一样。
		s.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyScheduleScan, Status: model.SchedulerDone,
			Scope: name,
			Message: "跳过一次错过的执行：服务停机期间已过点，刻意不补执行（计划 " +
				firedFor.Format("2006-01-02 15:04") + "，落后 " +
				now.Sub(firedFor).Round(time.Minute).String() + "）",
		})
		return
	}

	t, err := s.enqueue(ctx, sch)
	if err != nil {
		s.mark(ctx, sch.ID, "failed", nil)
		log.Printf("[schedule] 入队失败 id=%d action=%s: %v", sch.ID, sch.Action, err)
		s.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyScheduleScan, Status: model.SchedulerFailed,
			Scope:   name,
			Message: "触发定时任务失败：" + actionLabel(sch.Action) + "（" + err.Error() + "）",
		})
		return
	}
	s.mark(ctx, sch.ID, "success", &t.ID)

	log.Printf("[schedule] 已执行 id=%d vm=%d action=%s task=%d",
		sch.ID, sch.VMID, sch.Action, t.ID)
	s.obs.Record(ctx, scheduler.Event{
		Key: scheduler.KeyScheduleScan, Status: model.SchedulerDone,
		Scope:   name,
		Message: "触发定时任务：" + actionLabel(sch.Action) + "，任务 #" + strconv.FormatInt(t.ID, 10),
	})
}

// actionLabel 把动作翻译成人话。
//
// 事件消息是给用户看的，而存进库的是 `poweron` 这样的标识符——直接把标识符
// 拼进消息里，用户会看到「触发定时任务：poweron」，然后不确定自己是不是
// 设错了什么。
func actionLabel(action string) string {
	switch action {
	case model.ScheduleActionStart:
		return "开机"
	case model.ScheduleActionShutdown:
		return "关机"
	case model.ScheduleActionDelete:
		return "删除虚拟机"
	}
	return action
}

// enqueue 按动作入队一个任务。
//
// 参数用 map 而不是复用 vm 包的结构体：那会让本包依赖 vm 包的内部类型，
// 而两者的耦合点其实只有「任务参数的 JSON 字段名」这一点契约。
// 字段名变了这里会编译不过——不会，但测试会失败，足以拦住。
// SnapshotCreator 是创建快照的入口（由 vm 服务实现）。
//
// 定时任务只是"到点了替用户点一次按钮"，因此它复用同一条路径，而不是自己
// 拼任务参数。

type SnapshotCreator interface {
	CreateSnapshot(
		ctx context.Context, vmID int64, req vmsvc.CreateSnapshotRequest,
		v authz.Viewer, operatorName, clientIP string,
	) (*model.Task, error)
}

// SetSnapshotCreator 装配快照入口；不装配时定时快照动作会**报错**而不是静默跳过——
// 静默跳过的表现是"设了任务却从来没跑过"，那比显式失败难查得多。

func (s *Scheduler) SetSnapshotCreator(c SnapshotCreator) { s.snapshots = c }

// snapshotName 生成这次快照的名字。
//
// 用模板 + 时间戳而不是固定名字：固定名字会在第二次触发时撞唯一约束，
// 而那种失败看起来像"任务失败了"，用户会去看节点，而不是发现名字重复。
func snapshotName(sch model.VMSchedule) string {
	stamp := time.Now().Format("20060102-1504")
	base := ""
	if sch.SnapshotName != nil {
		base = strings.TrimSpace(*sch.SnapshotName)
	}
	if base == "" {
		return "auto-" + stamp
	}
	return base + "-" + stamp
}

func (s *Scheduler) enqueue(ctx context.Context, sch model.VMSchedule) (*model.Task, error) {
	vm, err := s.vmOf(ctx, sch.VMID)
	if err != nil {
		return nil, err
	}
	name := vm.Name

	spec := task.Spec{
		NodeID:       sch.NodeID,
		ResourceType: "vm",
		ResourceID:   sch.VMID,
		ResourceName: name,
		// 归属沿用虚拟机的所有者，而不是定时任务的创建者：管理员可能给
		// 别人的虚拟机设了一个定时任务，那次执行产生的任务应当归资源
		// 所有者可见，否则用户在自己的任务列表里看不到针对自己虚拟机的
		// 操作。
		OwnerID: ownerID(vm.OwnerID),
		// CreatedBy 为 0 时 Enqueue 会记成「无发起人」——这正是定时任务
		// 的实情：它由调度器触发，没有人在那一刻按按钮。
		CreatedBy: deref(sch.CreatedBy),
	}

	// 快照动作**不在这里拼任务参数**，而是调用虚拟机的创建快照入口。
	//
	// 原因很具体：`vm.snapshot.create` 需要先建一条快照记录拿到 ID，还要过配额、
	// 实时探测运行态才能决定用内部还是外部快照。在调度器里重抄一遍这套逻辑
	// 等于把规则放两份，而两份规则只在"配额超限"这个分支上分叉时就够查一天。
	//
	// 用接口换进来，调度器因此不依赖 vm 包，测试也不用构造整套虚拟机服务。
	if sch.Action == model.ScheduleActionSnapshot {
		if s.snapshots == nil {
			return nil, errors.New("未装配快照服务，定时快照无法执行")
		}
		return s.snapshots.CreateSnapshot(ctx, sch.VMID, vmsvc.CreateSnapshotRequest{
			Name:          snapshotName(sch),
			IncludeMemory: sch.IncludeMemory,
		}, authz.Viewer{UserID: ownerID(vm.OwnerID), IsAdmin: true}, "", "")
	}

	switch sch.Action {
	case model.ScheduleActionStart, model.ScheduleActionShutdown:
		spec.Type = model.TaskVMPower
		spec.Params = map[string]any{
			"vm_id":   sch.VMID,
			"vm_name": name,
			"action":  sch.Action,
			// 定时任务没有「受理时探测」这一步，因此没有观察到的状态。
			// 留空而不是编一个：事后排查时「为空」是准确的信息，
			// 填一个假状态会让人以为当时真的探测过。
			"observed_status": "",
		}

	case model.ScheduleActionDelete:
		spec.Type = model.TaskVMDelete
		spec.Params = map[string]any{
			"vm_id":   sch.VMID,
			"vm_name": name,
			// 定时删除**固定保留磁盘**（见 API 文档中的说明）：
			// 定时任务是无人值守的，而连盘删除的代价是数据永久丢失——
			// 用户设完就忘了，几周后数据静默消失，且没有任何人在那一刻
			// 能拦下。需要连盘删除时请手动执行，那一步有确认框。
			"disk_action":     "keep",
			"observed_status": "",
		}

	default:
		return nil, errUnknownAction
	}

	return s.queue.Enqueue(ctx, spec)
}

// vmNameOf 取虚拟机名，取不到时回落到 ID。
//
// 事件里的 scope 是给人看的，用名字才能直接对上是哪台机器——一个「虚拟机
// #17」的消息，用户还得再去查一次列表。
func (s *Scheduler) vmNameOf(ctx context.Context, vmID int64) string {
	if vm, err := s.vmOf(ctx, vmID); err == nil && vm.Name != "" {
		return vm.Name
	}
	return "虚拟机 #" + strconv.FormatInt(vmID, 10)
}

// vmOf 取虚拟机记录。
//
// 需要名字与归属两项：名字让节点侧不必回查控制面就能定位目标，也让任务列表
// 在虚拟机被删掉之后仍能显示操作对象是谁；归属决定这次执行产生的任务谁能看到。
func (s *Scheduler) vmOf(ctx context.Context, vmID int64) (*model.VM, error) {
	var vm model.VM
	if err := s.db.WithContext(ctx).
		Select("id", "name", "owner_id").
		First(&vm, vmID).Error; err != nil {
		return nil, err
	}
	return &vm, nil
}

// ownerID 把可空的归属转成任务入队需要的值（0 表示无归属）。
func ownerID(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// deref 把可空指针转成值（nil 表示 0）。
func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// mark 记录本次执行结果。
func (s *Scheduler) mark(ctx context.Context, id int64, result string, taskID *int64) {
	updates := map[string]any{"last_result": result}
	if taskID != nil {
		updates["last_task_id"] = *taskID
	}
	if err := s.db.WithContext(ctx).Model(&model.VMSchedule{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[schedule] 记录执行结果失败 id=%d: %v", id, err)
	}
}
