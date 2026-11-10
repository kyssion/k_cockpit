package passaudit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/ops/scheduler"
	"k_cockpit/internal/service/platform/passaudit"
)

// runLoop 启动周期检查，条件满足后停止，返回期间写入的调度事件。
func runLoop(t *testing.T, loop *passaudit.Loop, wait func(*gorm.DB) bool, db *gorm.DB) []model.SchedulerEvent {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Start(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if wait(db) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后循环没有退出")
	}

	var events []model.SchedulerEvent
	if err := db.Order("id").Find(&events).Error; err != nil {
		t.Fatalf("读取调度事件失败: %v", err)
	}
	return events
}

func countEvents(db *gorm.DB) int64 {
	var n int64
	db.Model(&model.SchedulerEvent{}).Count(&n)
	return n
}

// TestLoopStaysSilentWhenDisabled 是回归用例：开关关闭时**不能**记失败事件。
//
// 这条约定写在循环的注释里，但实现曾经漏掉——RunNow 对"未开启"返回一个
// 校验错误，循环把它当成了真失败，于是每台机器每次启动都会在调度事件页
// 留下一条红色的"口令检查失败：定时口令检查未开启"，而实际只是这个可选
// 能力没打开。写这条用例时才发现，修在 loop.go 的 tick 开头。
func TestLoopStaysSilentWhenDisabled(t *testing.T) {
	db := newDB(t)
	svc := passaudit.NewService(db, &fakeAgent{res: auditInfo(1)}, func() bool { return false }, nil)
	loop := passaudit.NewLoop(svc, passaudit.Options{Interval: time.Hour, Key: "security.password_audit"})
	loop.Observe(scheduler.NewRecorder(db, nil))

	events := runLoop(t, loop, func(*gorm.DB) bool { return false }, db)
	if len(events) != 0 {
		t.Fatalf("开关关闭时不应产生任何调度事件, 实际 %d 条: %+v", len(events), events)
	}
}

// TestLoopRecordsDoneWithCounts：完成事件要写清"检查了几个、命中几个"，
// 只写"成功"等于没说，用户看不出需不需要处理。
func TestLoopRecordsDoneWithCounts(t *testing.T) {
	db := newDB(t)
	mkNode(t, db, "n1", model.NodeEnrollEnrolled)
	mkUser(t, db, "alice", false)
	svc := passaudit.NewService(db, &fakeAgent{res: auditInfo(42,
		agentHit("alice"),
	)}, func() bool { return true }, nil)
	loop := passaudit.NewLoop(svc, passaudit.Options{Interval: time.Hour, Key: "security.password_audit"})
	loop.Observe(scheduler.NewRecorder(db, nil))

	if loop.Key() != "security.password_audit" {
		t.Errorf("Key() = %q", loop.Key())
	}

	events := runLoop(t, loop, func(db *gorm.DB) bool { return countEvents(db) > 0 }, db)
	if len(events) != 1 {
		t.Fatalf("应记 1 条事件, 实际 %d 条", len(events))
	}
	e := events[0]
	if e.Status != model.SchedulerDone {
		t.Errorf("状态 = %q, 期望 %q", e.Status, model.SchedulerDone)
	}
	if e.Message == nil || !strings.Contains(*e.Message, "42") || !strings.Contains(*e.Message, "命中 1") {
		t.Errorf("事件应写明账号数与命中数, 实际 %v", e.Message)
	}
}

// TestLoopRecordsFailureWhenNoNode：节点都没接入时必须留下失败事件——
// 这时"没检查"是运维要处理的问题，而不是静默跳过。
func TestLoopRecordsFailureWhenNoNode(t *testing.T) {
	db := newDB(t)
	svc := passaudit.NewService(db, &fakeAgent{}, func() bool { return true }, nil)
	loop := passaudit.NewLoop(svc, passaudit.Options{Interval: time.Hour, Key: "security.password_audit"})
	loop.Observe(scheduler.NewRecorder(db, nil))

	events := runLoop(t, loop, func(db *gorm.DB) bool { return countEvents(db) > 0 }, db)
	if len(events) != 1 {
		t.Fatalf("应记 1 条失败事件, 实际 %d 条", len(events))
	}
	if events[0].Status != model.SchedulerFailed {
		t.Errorf("状态 = %q, 期望 %q", events[0].Status, model.SchedulerFailed)
	}
	if events[0].Message == nil || !strings.Contains(*events[0].Message, "没有已接入的节点") {
		t.Errorf("失败原因应可读, 实际 %v", events[0].Message)
	}
}

// TestLoopWithoutRecorderDoesNotPanic：调度记录器是可选的（测试与 CLI
// 场景不装配它），缺了它不能崩。
func TestLoopWithoutRecorderDoesNotPanic(t *testing.T) {
	db := newDB(t)
	mkNode(t, db, "n1", model.NodeEnrollEnrolled)
	svc := passaudit.NewService(db, &fakeAgent{res: auditInfo(1)}, func() bool { return true }, nil)
	loop := passaudit.NewLoop(svc, passaudit.Options{Interval: 0, Key: "security.password_audit"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Start(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("循环未退出")
	}
}
