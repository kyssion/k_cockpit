package authkey_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/cryptoutil"
	"k_cockpit/internal/service/ops/scheduler"
	"k_cockpit/internal/service/platform/authkey"
)

// runLoop 启动循环并在条件满足（或超时）后停止，返回期间写入的调度事件。
//
// 循环是**阻塞**的（Start 直到 ctx 取消才返回），因此它必须跑在 goroutine 里；
// 测试用「等事件出现」而不是「睡固定时长」来判断，避免机器慢时假失败。
func runLoop(t *testing.T, loop *authkey.Loop, wait func(*gorm.DB) bool, db *gorm.DB) []model.SchedulerEvent {
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

// TestLoopRecordsEventOnlyWhenRotated 守住「只在真换了才记事件」这条约定：
// 绝大多数轮次是「没开自动轮换」，为它们各记一条会把调度事件页变成噪声。
func TestLoopRecordsEventOnlyWhenRotated(t *testing.T) {
	t.Run("关闭时不记事件", func(t *testing.T) {
		db := newDB(t)
		svc := authkey.NewService(db, rootA, bootstrap, nil)
		loop := authkey.NewLoop(svc, func() int { return 0 },
			authkey.Options{Interval: time.Hour, Key: "security.auth_key_rotate"})
		loop.Observe(scheduler.NewRecorder(db, nil))

		events := runLoop(t, loop, func(*gorm.DB) bool { return false }, db)
		if len(events) != 0 {
			t.Errorf("关闭自动轮换不应产生事件, 实际 %d 条: %+v", len(events), events)
		}
	})

	t.Run("到期轮换记一条完成事件", func(t *testing.T) {
		db := newDB(t)
		sealed, err := cryptoutil.Seal(rootA, bootstrap)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		old := time.Now().Add(-31 * 24 * time.Hour)
		if err := db.Create(&model.SessionSigningKey{
			KeyID: "0011223344556677", SecretEnc: sealed, Active: true,
			CreatedAt: old, RotatedAt: old,
		}).Error; err != nil {
			t.Fatalf("插入密钥失败: %v", err)
		}

		svc := authkey.NewService(db, rootA, bootstrap, nil)
		loop := authkey.NewLoop(svc, func() int { return 30 },
			authkey.Options{Interval: time.Hour, Key: "security.auth_key_rotate"})
		loop.Observe(scheduler.NewRecorder(db, nil))

		events := runLoop(t, loop, func(db *gorm.DB) bool { return countEvents(db) > 0 }, db)
		if len(events) != 1 {
			t.Fatalf("应恰好记 1 条事件, 实际 %d 条", len(events))
		}
		e := events[0]
		if e.SchedulerKey != "security.auth_key_rotate" {
			t.Errorf("事件 key = %q", e.SchedulerKey)
		}
		if e.Status != model.SchedulerDone {
			t.Errorf("事件状态 = %q, 期望 %q", e.Status, model.SchedulerDone)
		}
		// 事件文案要回答「动了什么」，不能只写「成功」。
		if e.Message == nil || !strings.Contains(*e.Message, "30") ||
			!strings.Contains(*e.Message, "失效") {
			t.Errorf("事件文案应说明轮换阈值与后果, 实际 %v", e.Message)
		}
	})

	t.Run("轮换失败记失败事件", func(t *testing.T) {
		db := newDB(t)
		sealed, err := cryptoutil.Seal(rootA, bootstrap)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		old := time.Now().Add(-31 * 24 * time.Hour)
		if err := db.Create(&model.SessionSigningKey{
			KeyID: "0011223344556677", SecretEnc: sealed, Active: true,
			CreatedAt: old, RotatedAt: old,
		}).Error; err != nil {
			t.Fatalf("插入密钥失败: %v", err)
		}

		// 故意用一把非法长度的加密密钥：轮换时 Seal 失败，但 Current（只读
		// 时间）仍可用——这正好只打中失败分支。
		svc := authkey.NewService(db, []byte("bad-key"), bootstrap, nil)
		loop := authkey.NewLoop(svc, func() int { return 30 },
			authkey.Options{Interval: time.Hour, Key: "security.auth_key_rotate"})
		loop.Observe(scheduler.NewRecorder(db, nil))

		events := runLoop(t, loop, func(db *gorm.DB) bool { return countEvents(db) > 0 }, db)
		if len(events) != 1 {
			t.Fatalf("应恰好记 1 条失败事件, 实际 %d 条", len(events))
		}
		if events[0].Status != model.SchedulerFailed {
			t.Errorf("事件状态 = %q, 期望 %q", events[0].Status, model.SchedulerFailed)
		}
		if events[0].Message == nil || !strings.Contains(*events[0].Message, "自动轮换失败") {
			t.Errorf("失败事件应说明原因, 实际 %v", events[0].Message)
		}
	})
}

// TestLoopUsesDefaultInterval 覆盖「传了非正间隔」的兜底：0 会让
// time.NewTicker panic，必须被换成默认值。
func TestLoopUsesDefaultInterval(t *testing.T) {
	db := newDB(t)
	svc := authkey.NewService(db, rootA, bootstrap, nil)
	loop := authkey.NewLoop(svc, func() int { return 0 }, authkey.Options{Interval: 0})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		loop.Start(ctx) // 若 Interval 为 0 这里会 panic
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("循环未退出")
	}
}
