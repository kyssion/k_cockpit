package scheduler_test

import (
	"context"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
)

func newEnv(t *testing.T) (*gorm.DB, *scheduler.Registry, *scheduler.Recorder, *scheduler.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "sched.db"),
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
	if err := db.AutoMigrate(&model.SchedulerEvent{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	reg := scheduler.NewRegistry()
	return db, reg, scheduler.NewRecorder(db, reg), scheduler.NewService(db, reg)
}

func countEvents(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	db.Model(&model.SchedulerEvent{}).Count(&n)
	return n
}

// TestNoActionNoEvent 覆盖这张表最核心的约束（F-7-04）。
//
// 一个每 30 秒醒一次的调度器，一天会醒 2880 次。如果每次都写一条「检查过，
// 无事发生」，那么真正有价值的那一条——「清理了 500 个过期任务」——会淹没在
// 两千多条噪声里，而用户翻事件列表的目的恰恰是找它。
func TestNoActionNoEvent(t *testing.T) {
	db, _, _, _ := newEnv(t)

	// 一个什么都没做的调度器：不调用 Record。
	// 事件表应当保持为空——这正是「没有动作」应有的结果。
	if n := countEvents(t, db); n != 0 {
		t.Errorf("没有动作时不该有事件，实际 %d 条", n)
	}
}

// TestOneEventPerRoundNotPerNode 覆盖记录粒度。
//
// 采集器一轮扫 20 台节点，若每台记一条，一天就是两万多条「我采到了」。
// 记录的应当是**这一轮实际采到了东西**这件事本身。
func TestOneEventPerRoundNotPerNode(t *testing.T) {
	db, reg, rec, _ := newEnv(t)
	ctx := context.Background()
	reg.Register(scheduler.Info{
		Key: scheduler.KeyMetricsHost, Name: "宿主机指标采集", Group: scheduler.GroupMetrics,
	})

	// 模拟 20 台节点的采集结果：**一条事件**。
	rec.Record(ctx, scheduler.Event{
		Key: scheduler.KeyMetricsHost, Status: model.SchedulerDone,
		Scope: "20 个节点", Message: "采集宿主机指标",
	})

	if n := countEvents(t, db); n != 1 {
		t.Errorf("一轮应记 1 条，实际 %d 条", n)
	}
}

// TestOverviewIncludesSchedulersWithoutEvents 覆盖「没有事件 ≠ 没在运行」。
//
// 注册表是「有哪些东西在跑」的答案，而事件表只在该做事的时候才有行。
// 只返回有事件的那些，会让一个正常但最近没事可做的调度器凭空消失。
func TestOverviewIncludesSchedulersWithoutEvents(t *testing.T) {
	db, reg, rec, svc := newEnv(t)
	ctx := context.Background()

	reg.Register(scheduler.Info{Key: "a", Name: "甲", Group: "组一", IntervalSeconds: 30})
	reg.Register(scheduler.Info{Key: "b", Name: "乙", Group: "组一", IntervalSeconds: 60})
	_ = db

	// 只有 a 有事件。
	rec.Record(ctx, scheduler.Event{Key: "a", Status: model.SchedulerDone, Message: "做了一件事"})

	views, err := svc.Overview(ctx, 5)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("应返回 2 个调度器（含没有事件的），实际 %d 个", len(views))
	}
	byKey := map[string]scheduler.SchedulerView{}
	for _, v := range views {
		byKey[v.Info.Key] = v
	}
	if len(byKey["a"].Events) != 1 {
		t.Errorf("a 应有 1 条事件，实际 %d", len(byKey["a"].Events))
	}
	if len(byKey["b"].Events) != 0 {
		t.Errorf("b 不该有事件，实际 %d", len(byKey["b"].Events))
	}
	if byKey["b"].LastActionAt != "" {
		t.Error("没有过动作时 LastActionAt 应为空——它表示「还没有过动作」，不是「没在运行」")
	}
	// 周期必须带出来：界面上没有别的字段能告诉用户「多久做一次」。
	if byKey["b"].Info.IntervalSeconds != 60 {
		t.Errorf("周期 = %d, 期望 60", byKey["b"].Info.IntervalSeconds)
	}
	if !byKey["b"].Info.RecordsOnlyOnAction {
		t.Error("应标明「只在有动作时记录」，否则空事件会被读成故障")
	}
}

func TestEventsFilterAndFailedCount(t *testing.T) {
	_, reg, rec, svc := newEnv(t)
	ctx := context.Background()
	reg.Register(scheduler.Info{Key: "k", Name: "K", Group: "G"})

	rec.Record(ctx, scheduler.Event{Key: "k", Status: model.SchedulerDone, Message: "成功"})
	rec.Record(ctx, scheduler.Event{Key: "k", Status: model.SchedulerFailed, Message: "失败"})

	views, err := svc.Overview(ctx, 5)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("应有 1 个调度器，实际 %d", len(views))
	}
	// 失败数要单独给出来：一个偶尔失败的调度器如果只显示「最近一次成功」，
	// 用户就看不到它在反复失败。
	if views[0].FailedCount != 1 {
		t.Errorf("失败数 = %d, 期望 1", views[0].FailedCount)
	}

	only, err := svc.Events(ctx, "k", model.SchedulerFailed, 10)
	if err != nil {
		t.Fatalf("筛选失败: %v", err)
	}
	if len(only) != 1 || only[0].Status != model.SchedulerFailed {
		t.Errorf("按失败筛选应得到 1 条，实际 %d 条", len(only))
	}
}

// TestEventKeepsNameAtTheTime 覆盖名字的冗余存储。
//
// 调度器是代码里的常量而不是表里的行，名字改了之后历史事件应当保留**当时**
// 那个名字——「当时它叫什么」是历史的一部分。
func TestEventKeepsNameAtTheTime(t *testing.T) {
	_, reg, rec, svc := newEnv(t)
	ctx := context.Background()

	reg.Register(scheduler.Info{Key: "k", Name: "旧名字", Group: "G"})
	rec.Record(ctx, scheduler.Event{Key: "k", Status: model.SchedulerDone})

	// 改名（重新注册即覆盖）。
	reg.Register(scheduler.Info{Key: "k", Name: "新名字", Group: "G"})

	views, err := svc.Overview(ctx, 5)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(views[0].Events) != 1 {
		t.Fatalf("应有 1 条事件")
	}
	if views[0].Events[0].Name != "旧名字" {
		t.Errorf("历史事件应保留当时的名字，实际 %q", views[0].Events[0].Name)
	}
	if views[0].Info.Name != "新名字" {
		t.Errorf("注册表应显示新名字，实际 %q", views[0].Info.Name)
	}
}

func TestUnknownStatusFallsBackToDone(t *testing.T) {
	_, reg, rec, svc := newEnv(t)
	ctx := context.Background()
	reg.Register(scheduler.Info{Key: "k", Name: "K"})

	rec.Record(ctx, scheduler.Event{Key: "k", Status: "什么鬼"})

	views, _ := svc.Overview(ctx, 5)
	if len(views[0].Events) != 1 {
		t.Fatalf("应有 1 条事件")
	}
	if views[0].Events[0].Status != model.SchedulerDone {
		t.Errorf("未知状态应回落到 done，实际 %q", views[0].Events[0].Status)
	}
}

// TestRecordWithoutRegistryStillWorks 覆盖健壮性。
//
// 记录失败或注册表缺失**不该影响被观测的动作**——观测是附加能力，
// 它不能反过来改变被观测的东西。
func TestRecordWithoutRegistryStillWorks(t *testing.T) {
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "s.db"),
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
	if err := db.AutoMigrate(&model.SchedulerEvent{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 注册表里没有这个 key，仍然要能记录（名字留空）。
	rec := scheduler.NewRecorder(db, scheduler.NewRegistry())
	rec.Record(context.Background(), scheduler.Event{
		Key: "未登记", Status: model.SchedulerDone, Message: "仍然记下来了",
	})
	if n := countEvents(t, db); n != 1 {
		t.Errorf("未登记的 key 也应能记录，实际 %d 条", n)
	}

	// nil 记录器不 panic。
	var nilRec *scheduler.Recorder
	nilRec.Record(context.Background(), scheduler.Event{Key: "x"})
}

func TestRegisterBuiltinsCoversAllKeys(t *testing.T) {
	reg := scheduler.NewRegistry()
	scheduler.RegisterBuiltins(reg, scheduler.BuiltinOptions{})

	for _, key := range []string{
		scheduler.KeyMetricsHost, scheduler.KeyMetricsGuest,
		scheduler.KeyMetricsDaily, scheduler.KeyScheduleScan, scheduler.KeyTaskQueue,
	} {
		info, ok := reg.Get(key)
		if !ok {
			t.Errorf("内置调度器 %s 未登记", key)
			continue
		}
		// 说明是界面上唯一的解释来源，不能为空。
		if info.Name == "" || info.Description == "" || info.Group == "" {
			t.Errorf("%s 的名称/分组/说明不能为空", key)
		}
	}
}
