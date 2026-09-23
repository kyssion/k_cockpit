package schedule

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"

	"gorm.io/gorm"
)

// --- 时间计算 ---

func TestNextAfterDaily(t *testing.T) {
	runAt := "03:00"
	sch := &model.VMSchedule{ScheduleType: model.ScheduleTypeDaily, RunAt: &runAt, Enabled: true}

	from := time.Date(2026, 9, 16, 1, 0, 0, 0, time.Local)
	got := NextAfter(sch, from)
	if got == nil || got.Hour() != 3 || got.Day() != 16 {
		t.Errorf("当天未到点 → 期望当天 03:00, 得到 %v", got)
	}

	from = time.Date(2026, 9, 16, 4, 0, 0, 0, time.Local)
	got = NextAfter(sch, from)
	if got == nil || got.Hour() != 3 || got.Day() != 17 {
		t.Errorf("当天已过点 → 期望次日 03:00, 得到 %v", got)
	}

	// **恰好等于执行时刻**时必须返回次日。
	//
	// 这是「严格晚于」而非「不早于」的分界：调度器在 03:00 处理完一轮后
	// 立刻算下一次，若返回同一个 03:00，这一轮就会反复触发同一条记录。
	from = time.Date(2026, 9, 16, 3, 0, 0, 0, time.Local)
	got = NextAfter(sch, from)
	if got == nil || got.Day() != 17 {
		t.Errorf("恰好到点 → 期望次日, 得到 %v", got)
	}
}

func TestNextAfterWeekly(t *testing.T) {
	runAt := "09:30"
	days := "1,3,5" // 周一、三、五
	sch := &model.VMSchedule{
		ScheduleType: model.ScheduleTypeWeekly,
		RunAt:        &runAt, Weekdays: &days, Enabled: true,
	}

	// 基准：2026-09-16 应为周三（下面的断言会先确认这一点，
	// 否则整个测试的日期推算都建立在错误的假设上）。
	wed := time.Date(2026, 9, 16, 0, 0, 0, 0, time.Local)
	if wed.Weekday() != time.Wednesday {
		t.Fatalf("测试基准算错了：2026-09-16 是 %v，期望周三", wed.Weekday())
	}

	got := NextAfter(sch, time.Date(2026, 9, 16, 8, 0, 0, 0, time.Local))
	if got == nil || got.Day() != 16 || got.Hour() != 9 || got.Minute() != 30 {
		t.Errorf("周三未到点 → 期望当天 09:30, 得到 %v", got)
	}

	// 跳过不在选中集合里的周四。
	got = NextAfter(sch, time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local))
	if got == nil || got.Day() != 18 {
		t.Errorf("周三已过点 → 期望周五(18), 得到 %v", got)
	}

	// 跨周：周五之后回到下周一。
	got = NextAfter(sch, time.Date(2026, 9, 18, 10, 0, 0, 0, time.Local))
	if got == nil || got.Day() != 21 {
		t.Errorf("周五已过点 → 期望下周一(21), 得到 %v", got)
	}
}

func TestNextAfterStops(t *testing.T) {
	runAt := "03:00"

	// 一次性任务没有「下一次」。
	once := &model.VMSchedule{ScheduleType: model.ScheduleTypeOnce, RunAt: &runAt, Enabled: true}
	if got := NextAfter(once, time.Now()); got != nil {
		t.Errorf("一次性任务应返回 nil, 得到 %v", got)
	}

	// 停用任务在任何模式下都不再执行。
	disabled := &model.VMSchedule{ScheduleType: model.ScheduleTypeDaily, RunAt: &runAt, Enabled: false}
	if got := NextAfter(disabled, time.Now()); got != nil {
		t.Errorf("停用任务应返回 nil, 得到 %v", got)
	}

	// 每周模式却没勾任何一天：不编一个默认值代替。
	// 「猜用户想要哪天」比不执行更危险——它会做出一个用户从没要求过的操作。
	empty := ""
	noDays := &model.VMSchedule{
		ScheduleType: model.ScheduleTypeWeekly, RunAt: &runAt, Weekdays: &empty, Enabled: true,
	}
	if got := NextAfter(noDays, time.Now()); got != nil {
		t.Errorf("未选任何星期时应返回 nil, 得到 %v", got)
	}
}

// TestWeekdayNumberingIsISO 覆盖一处最容易错位的地方。
//
// 用 ISO 编号（1=周一 … 7=周日）而不是 Go 的 0=周日：后者会让「周日」在
// 界面上排到第一位，而用户按自然顺序读「周一到周日」。两套编号混用是这类
// 功能最常见的错误来源，且**只在勾选周日时才会暴露**——平时完全正常。
func TestWeekdayNumberingIsISO(t *testing.T) {
	cases := map[int]time.Weekday{
		1: time.Monday, 2: time.Tuesday, 3: time.Wednesday, 4: time.Thursday,
		5: time.Friday, 6: time.Saturday, 7: time.Sunday,
	}

	base := time.Date(2026, 9, 14, 0, 0, 0, 0, time.Local) // 周一
	if base.Weekday() != time.Monday {
		t.Fatalf("基准日期算错：2026-09-14 是 %v", base.Weekday())
	}

	for iso, want := range cases {
		day := base
		for day.Weekday() != want {
			day = day.AddDate(0, 0, 1)
		}
		if got := isoWeekday(day); got != iso {
			t.Errorf("%v 的 ISO 编号 = %d, 期望 %d", want, got, iso)
		}
	}

	raw := "7"
	days := parseWeekdays(&raw)
	if !days[7] {
		t.Error("周日应解析为 7")
	}
	if days[0] {
		t.Error("不应存在编号 0——ISO 里周日是 7，用 0 会让周日与「无」混淆")
	}
}

func TestParseWeekdaysIgnoresInvalid(t *testing.T) {
	// 单个非法值不该让整条记录失效：只忽略它，其余照常。
	raw := "1,abc,9,3,,5"
	days := parseWeekdays(&raw)

	for _, d := range []int{1, 3, 5} {
		if !days[d] {
			t.Errorf("缺少星期 %d", d)
		}
	}
	if len(days) != 3 {
		t.Errorf("解析出 %d 天, 期望 3（无效值应被忽略而不是让它整体失败）", len(days))
	}
}

// --- 服务层 ---

func newTestEnv(t *testing.T) (*Service, *Scheduler, *task.Queue, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "schedule.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.VM{}, &model.Task{}, &model.AuditLog{}, &model.VMSchedule{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	if err := db.Create(&model.VM{
		ID: 1, NodeID: 1, Name: "vm-sched", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)),
	}).Error; err != nil {
		t.Fatalf("创建测试虚拟机失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 1, PollInterval: 20 * time.Millisecond,
	})
	// 注册替身执行器：Enqueue 会校验任务类型是否已注册，但本包测试只关心
	// 「任务被创建成什么样」，不关心它怎么执行——那由 vm 包自己的测试覆盖。
	// 队列**不启动**：不启动时任务停在 pending，正好让我们只看入队结果，
	// 也不必处理启停带来的时序问题。
	queue.Register(fakeExecutor{typ: model.TaskVMPower})
	queue.Register(fakeExecutor{typ: model.TaskVMDelete})

	return NewService(db), New(db, queue, Options{}), queue, db
}

// fakeExecutor 是一个不执行任何操作的执行器，仅用于通过入队时的类型校验。
type fakeExecutor struct{ typ string }

func (e fakeExecutor) Type() string                           { return e.typ }
func (e fakeExecutor) Run(context.Context, *model.Task) error { return nil }

func TestDeleteScheduleMustBeOneTime(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	// 一次性删除是允许的。
	if _, err := svc.Create(ctx, 1, CreateRequest{
		Action: model.ScheduleActionDelete, ScheduleType: model.ScheduleTypeOnce,
		Date: "2030-01-01", TimeOfDay: "03:00",
	}, viewer); err != nil {
		t.Fatalf("一次性删除任务应被接受: %v", err)
	}

	// 周期删除必须被拒绝。
	//
	// 这条限制针对的是一类具体事故：周期删除任务设完就被遗忘，直到某天
	// 虚拟机连同数据一起消失，而那一刻没有任何人知道发生了什么。
	for _, kind := range []string{model.ScheduleTypeDaily, model.ScheduleTypeWeekly} {
		_, err := svc.Create(ctx, 1, CreateRequest{
			Action: model.ScheduleActionDelete, ScheduleType: kind,
			Weekdays: []int{1, 3}, TimeOfDay: "03:00",
		}, viewer)
		assertAPIError(t, err, 422)
	}
}

func TestCreateRejectsPastTime(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	// 过去的时间点必须拒绝，而不是静默改成明天执行——
	// 那会做出一个用户从未要求过的操作。
	_, err := svc.Create(ctx, 1, CreateRequest{
		Action: model.ScheduleActionShutdown, ScheduleType: model.ScheduleTypeOnce,
		Date: "2020-01-01", TimeOfDay: "03:00",
	}, authz.Viewer{UserID: 7})
	assertAPIError(t, err, 422)
}

func TestCreateRejectsBadTimeFormat(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	// 把 `25:00` 存成 `00:00` 会让任务在一个用户从未指定的时刻执行。
	for _, bad := range []string{"", "25:00", "3", "abc", "12:99"} {
		_, err := svc.Create(ctx, 1, CreateRequest{
			Action: model.ScheduleActionStart, ScheduleType: model.ScheduleTypeDaily,
			TimeOfDay: bad,
		}, authz.Viewer{UserID: 7})
		if err == nil {
			t.Errorf("时刻 %q 应被拒绝", bad)
		}
	}
}

func TestCreateRejectsOthersVM(t *testing.T) {
	svc, _, _, db := newTestEnv(t)
	ctx := context.Background()

	db.Create(&model.VM{ID: 2, NodeID: 1, Name: "vm-theirs", OwnerID: ptr(int64(20))})

	_, err := svc.Create(ctx, 2, CreateRequest{
		Action: model.ScheduleActionStart, ScheduleType: model.ScheduleTypeDaily,
		TimeOfDay: "03:00",
	}, authz.Viewer{UserID: 10})
	assertAPIError(t, err, 404)
}

// --- 调度器 ---

func TestSchedulerFiresDueTask(t *testing.T) {
	_, sched, _, db := newTestEnv(t)
	ctx := context.Background()

	runAt := "03:00"
	due := time.Now().Add(-time.Minute)
	db.Create(&model.VMSchedule{
		VMID: 1, NodeID: 1,
		Action: model.ScheduleActionShutdown, ScheduleType: model.ScheduleTypeDaily,
		RunAt: &runAt, NextRunAt: &due, Enabled: true,
	})

	sched.tick(ctx)

	// 应产生一个 vm.power 任务，并把下次执行时间推到明天。
	var tasks []model.Task
	db.Find(&tasks)
	if len(tasks) != 1 || tasks[0].Type != model.TaskVMPower {
		t.Fatalf("任务 = %+v, 期望一个 vm.power", tasks)
	}

	var after model.VMSchedule
	db.First(&after, "vm_id = ?", 1)
	if after.NextRunAt == nil || !after.NextRunAt.After(time.Now()) {
		t.Errorf("下次执行时间未推进: %v", after.NextRunAt)
	}
	if after.LastResult == nil || *after.LastResult != "success" {
		t.Errorf("执行结果 = %v, 期望 success", after.LastResult)
	}
	if after.LastTaskID == nil {
		t.Error("未记录产生的任务标识")
	}
}

// TestSchedulerSkipsMissedRuns 覆盖「服务停机期间错过的时间点」。
//
// **刻意不补执行**：补执行意味着恢复后连着做几次本该分散在不同时间的操作。
// 对「每天 3 点关机」无害，但对「2 点开机、3 点关机」，补执行会让虚拟机刚
// 开机就被关掉——用户看到「它自己开了又关」，而原因埋在几天前的停机里。
func TestSchedulerSkipsMissedRuns(t *testing.T) {
	_, sched, _, db := newTestEnv(t)
	ctx := context.Background()

	runAt := "03:00"
	missed := time.Now().Add(-3 * time.Hour)
	db.Create(&model.VMSchedule{
		VMID: 1, NodeID: 1,
		Action: model.ScheduleActionShutdown, ScheduleType: model.ScheduleTypeDaily,
		RunAt: &runAt, NextRunAt: &missed, Enabled: true,
	})

	sched.tick(ctx)

	var count int64
	db.Model(&model.Task{}).Count(&count)
	if count != 0 {
		t.Errorf("错过的时间点不应补执行，但产生了 %d 个任务", count)
	}

	var after model.VMSchedule
	db.First(&after, "vm_id = ?", 1)
	if after.LastResult == nil || *after.LastResult != "skipped" {
		t.Errorf("执行结果 = %v, 期望 skipped（跳过必须在界面上留痕，"+
			"否则用户会以为任务没跑过而反复重建）", after.LastResult)
	}
	if after.NextRunAt == nil || !after.NextRunAt.After(time.Now()) {
		t.Errorf("跳过之后也应推进到下一个时间点: %v", after.NextRunAt)
	}
}

func TestSchedulerIgnoresDisabled(t *testing.T) {
	_, sched, _, db := newTestEnv(t)
	ctx := context.Background()

	runAt := "03:00"
	due := time.Now().Add(-time.Minute)

	// 停用必须走 Update：Enabled 带 default:true，直接 Create 传 false 会被
	// GORM 当作零值省略、由数据库填成 true（模型上有详细说明）。这里用两步
	// 写正是为了绕开那个陷阱——顺带让这条测试也成为该行为的回归防线。
	row := model.VMSchedule{
		VMID: 1, NodeID: 1,
		Action: model.ScheduleActionShutdown, ScheduleType: model.ScheduleTypeDaily,
		RunAt: &runAt, NextRunAt: &due,
	}
	db.Create(&row)
	if err := db.Model(&model.VMSchedule{}).Where("id = ?", row.ID).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("停用任务失败: %v", err)
	}

	sched.tick(ctx)

	var count int64
	db.Model(&model.Task{}).Count(&count)
	if count != 0 {
		t.Errorf("已停用的任务被执行了 %d 次", count)
	}
}

func ptr[T any](v T) *T { return &v }

func assertAPIError(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 %d 错误，实际为 nil", status)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型 = %T, 期望 *api.Error", err)
	}
	if apiErr.Status != status {
		t.Errorf("状态码 = %d, 期望 %d（消息: %s）", apiErr.Status, status, apiErr.Message)
	}
}
