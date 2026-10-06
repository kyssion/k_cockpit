package task_test

import (
	"context"
	"testing"
	"time"

	"k_cockpit/internal/model"
)

// TestCleanupKeepsReferencedTasks 覆盖清理最容易被忽略的一条。
//
// **被引用的任务不删。** `vm_schedule.last_task_id` 是「这条定时任务上次跑的
// 结果」——它是**当前状态的一部分**，不是历史。清掉它指向的任务之后，定时
// 任务页上的「上次执行」就永远显示不出来了；而用户看不出是「任务被清理了」
// 还是「记录坏了」。
func TestCleanupKeepsReferencedTasks(t *testing.T) {
	q, db := newTestQueue(t)
	ctx := context.Background()

	old := time.Now().AddDate(0, 0, -30)
	mk := func(name string) model.Task {
		tk := model.Task{
			Type: "vm.start", Status: model.TaskSuccess, ResourceName: &name,
			FinishedAt: &old,
		}
		if err := db.Create(&tk).Error; err != nil {
			t.Fatalf("造任务失败: %v", err)
		}
		return tk
	}
	referenced := mk("referenced")
	byCapture := mk("by-capture")
	plain := mk("plain")

	// 一条定时任务指着 referenced，一条抓包记录指着 by-capture。
	if err := db.Create(&model.VMSchedule{
		VMID: 1, NodeID: 1, Action: model.ScheduleActionStart,
		LastTaskID: &referenced.ID,
	}).Error; err != nil {
		t.Fatalf("造定时任务失败: %v", err)
	}
	if err := db.Create(&model.NetworkCapture{
		NodeID: 1, TaskID: &byCapture.ID,
	}).Error; err != nil {
		t.Fatalf("造抓包记录失败: %v", err)
	}

	n, err := q.Cleanup(ctx, time.Now())
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应只清掉 1 条（无引用的那条），实际 %d", n)
	}

	for _, id := range []int64{referenced.ID, byCapture.ID} {
		var cnt int64
		db.Model(&model.Task{}).Where("id = ?", id).Count(&cnt)
		if cnt != 1 {
			t.Errorf("任务 %d 被引用了，不该被清掉", id)
		}
	}
	var cnt int64
	db.Model(&model.Task{}).Where("id = ?", plain.ID).Count(&cnt)
	if cnt != 0 {
		t.Error("无引用的终态任务应当被清掉")
	}
}

// TestCleanupKeepsRunningTasks 覆盖「执行中的不清」。
//
// 删除一个执行中的任务会让它的结果永远无处落定——而那条结果正是用户在等的。
func TestCleanupKeepsRunningTasks(t *testing.T) {
	q, db := newTestQueue(t)
	ctx := context.Background()

	old := time.Now().AddDate(0, 0, -30)
	running := model.Task{Type: "vm.start", Status: model.TaskRunning, FinishedAt: &old}
	pending := model.Task{Type: "vm.start", Status: model.TaskPending}
	db.Create(&running)
	db.Create(&pending)

	n, err := q.Cleanup(ctx, time.Now())
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 0 {
		t.Errorf("执行中与待执行的任务都不该被清掉，实际清了 %d", n)
	}
	var cnt int64
	db.Model(&model.Task{}).Count(&cnt)
	if cnt != 2 {
		t.Errorf("两条都应还在，实际剩 %d", cnt)
	}
}

// TestCleanupRespectsCutoff 覆盖时间线。
//
// 只清「在某时刻之前完成的」——这是用户点「清理」时的实际语义（"清掉旧的"），
// 而不是「把刚才那条也删了」。
func TestCleanupRespectsCutoff(t *testing.T) {
	q, db := newTestQueue(t)
	ctx := context.Background()

	old := time.Now().AddDate(0, 0, -30)
	recent := time.Now().Add(-time.Minute)
	db.Create(&model.Task{Type: "vm.start", Status: model.TaskSuccess, FinishedAt: &old})
	db.Create(&model.Task{Type: "vm.start", Status: model.TaskSuccess, FinishedAt: &recent})

	cutoff := time.Now().AddDate(0, 0, -7)
	n, err := q.Cleanup(ctx, cutoff)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应只清掉 30 天前的那条，实际 %d", n)
	}
	var cnt int64
	db.Model(&model.Task{}).Count(&cnt)
	if cnt != 1 {
		t.Errorf("应剩 1 条，实际 %d", cnt)
	}
}
