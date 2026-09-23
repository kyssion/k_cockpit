package capture_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/capture"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *capture.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "cap.db"),
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
		&model.NetworkCapture{}, &model.Node{}, &model.VM{},
		&model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(capture.NewExecutor(db, client))
	q.Register(capture.NewDeleteExecutor(db, client))
	return db, capture.NewService(db, client, recorder, q)
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}

// TestFilterRejectsInjection 覆盖本块最重要的一处安全校验。
//
// Filter 最终会被拼进 tcpdump 的命令行，因此它是一处**注入面**。校验放在
// 控制面而不是只靠节点侧转义：节点侧的转义是第二道防线，不是第一道，
// 而"只靠下游小心处理"这种安排在下游某次重构之后就悄悄失效了。
func TestFilterRejectsInjection(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	bad := []string{
		`tcp port 80; rm -rf /`,
		"tcp port 80 && reboot",
		`tcp port 80 | tee /etc/passwd`,
		"tcp port 80 `id`",
		"tcp port 80 $(whoami)",
		`tcp port 80 > /etc/crontab`,
		"tcp port 80\nrm -rf /",
		`tcp port 80 '`,
		`tcp port 80 "`,
		`tcp port 80 \`,
	}
	for _, f := range bad {
		_, _, err := svc.Start(ctx, capture.Request{
			NodeID: 1, Interface: "vnet0", Filter: f, DurationSec: 30,
		}, admin(), "root", "")
		if err == nil {
			t.Errorf("过滤器 %q 应被拒绝", f)
		}
	}
}

// TestFilterAcceptsCommonExpressions 覆盖**只测拒绝是不够的**。
//
// 一组只测拒绝的用例拦不住一个"全都拒绝"的实现，而那样滤器功能就等于没有。
func TestFilterAcceptsCommonExpressions(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	good := []string{
		"", // 空 = 抓全部流量，是合法且常见的用法
		"tcp port 80",
		"host 10.0.0.5",
		"udp portrange 1000-2000",
		"tcp and port 443",
		"not port 22",
	}
	for _, f := range good {
		view, _, err := svc.Start(ctx, capture.Request{
			NodeID: 1, Interface: "vnet0", Filter: f, DurationSec: 30,
		}, admin(), "root", "")
		if err != nil {
			t.Errorf("过滤器 %q 应被接受，实际 %v", f, err)
		}
		// 每个用例之后清掉记录，否则会撞上并发上限而把**过滤器**的失败
		// 掩盖成"节点忙"——那样这个用例就测不到它想测的东西了。
		if view != nil {
			if _, err := svc.Delete(ctx, view.ID, admin(), "root", ""); err != nil {
				t.Fatalf("清理失败: %v", err)
			}
		}
	}
}

// TestDurationRequired 覆盖"抓包必须有时限"。
//
// 不限时的抓包会把宿主机磁盘写满，而且用户会忘记停——它的失败不是
// "没抓到"，而是"把宿主机写挂了"。
func TestDurationRequired(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	for _, d := range []int{0, -1, 3} {
		_, _, err := svc.Start(ctx, capture.Request{
			NodeID: 1, Interface: "vnet0", DurationSec: d,
		}, admin(), "root", "")
		assertStatus(t, err, 400)
	}

	_, _, err := svc.Start(ctx, capture.Request{
		NodeID: 1, Interface: "vnet0", DurationSec: 3600,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
	if err != nil && !strings.Contains(err.Error(), "写满") {
		t.Errorf("报错应说清为什么不能无限抓：%v", err)
	}
}

func TestInterfaceRequired(t *testing.T) {
	_, svc := newEnv(t)

	_, _, err := svc.Start(context.Background(), capture.Request{
		NodeID: 1, Interface: "  ", DurationSec: 30,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

// TestConcurrencyCapped 覆盖并发上限。
//
// 抓包本身占 CPU 与磁盘带宽，而它是在**生产节点**上跑。允许无限并发的话，
// 几个人同时点"抓包"就足以影响那台机器上虚拟机的网络性能——而他们各自的
// 界面都显示"一切正常"。
func TestConcurrencyCapped(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	// 抓包还没生成文件，因此都算"进行中"（测试里的队列没有后台循环）。
	for i := 0; i < 3; i++ {
		if _, _, err := svc.Start(ctx, capture.Request{
			NodeID: 1, Interface: "vnet0", DurationSec: 30,
		}, admin(), "root", ""); err != nil {
			t.Fatalf("第 %d 个抓包应被接受: %v", i+1, err)
		}
	}
	_, _, err := svc.Start(ctx, capture.Request{
		NodeID: 1, Interface: "vnet1", DurationSec: 30,
	}, admin(), "root", "")
	assertStatus(t, err, 409)
}

// TestRunningShowsCountdown 覆盖异步的呈现。
//
// 抓包下发之后要等 duration 秒才有文件。这期间界面必须显示"进行中"，
// 而不是"文件不存在"——后者会让用户以为抓包失败了。
func TestRunningShowsCountdown(t *testing.T) {
	_, svc := newEnv(t)

	view, _, err := svc.Start(context.Background(), capture.Request{
		NodeID: 1, Interface: "vnet0", DurationSec: 60,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("发起失败: %v", err)
	}
	if view.Status != "running" {
		t.Errorf("刚发起时状态 = %q, 期望 running", view.Status)
	}
	// 倒计时由服务端算，不让界面拿时刻减本地时间——客户端时钟不准是常态。
	if view.SecondsLeft <= 0 || view.SecondsLeft > 60 {
		t.Errorf("剩余秒数 = %d, 应在 0~60 之间", view.SecondsLeft)
	}
}

// TestEmptyFileExplainsItself 覆盖"抓到 0 字节"。
//
// 这是一个**看不出原因**的结果，而用户会先去怀疑抓包功能坏了。最常见的原因
// 是过滤器没匹配到流量——必须直接说出来。
func TestEmptyFileExplainsItself(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	view, tk, err := svc.Start(ctx, capture.Request{
		NodeID: 1, Interface: "vnet0", Filter: "host 10.0.0.5", DurationSec: 10,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("发起失败: %v", err)
	}
	_ = db

	// 驱动执行器（测试里的队列没有后台循环）。
	var task2 model.Task
	if err := db.First(&task2, tk.ID).Error; err != nil {
		t.Fatalf("找不到任务: %v", err)
	}
	if err := capture.NewExecutor(db, agent.NewMockClient()).Run(ctx, &task2); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	var row model.NetworkCapture
	if err := db.First(&row, view.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !row.Ready() {
		t.Fatal("执行后应已生成文件")
	}
	if row.SizeBytes != 0 {
		t.Fatalf("这个过滤器的 mock 结果应为空，实际 %d", row.SizeBytes)
	}

	items, err := svc.List(ctx, 1, admin())
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if items[0].Status != "ready" {
		t.Errorf("状态 = %q, 期望 ready", items[0].Status)
	}
	if items[0].Hint == "" {
		t.Error("空文件应给出原因提示——它本身看不出是过滤器的问题还是抓包坏了")
	}
}

// TestTenantSeesOnlyOwnCaptures 覆盖隔离。
//
// 抓包内容含明文流量，而一个网口上可能同时有多个租户的机器。
func TestTenantSeesOnlyOwnCaptures(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	alice := authz.Viewer{UserID: 1}
	bob := authz.Viewer{UserID: 2}

	if _, _, err := svc.Start(ctx, capture.Request{
		NodeID: 1, Interface: "vnet0", DurationSec: 30,
	}, alice, "alice", ""); err != nil {
		t.Fatalf("alice 发起失败: %v", err)
	}

	items, err := svc.List(ctx, 1, bob)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("bob 不该看到 alice 的抓包，实际 %d 条", len(items))
	}

	// alice 自己能看到。
	items, _ = svc.List(ctx, 1, alice)
	if len(items) != 1 {
		t.Errorf("alice 应看到自己的 1 条，实际 %d", len(items))
	}
}

// TestDeleteSendsFileRemovalToNode 覆盖"文件与控制面记录一起删"。
//
// 只删记录会让一份含明文流量的文件永远留在宿主机上，而界面上已经看不到它。
func TestDeleteSendsFileRemovalToNode(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	view, tk, err := svc.Start(ctx, capture.Request{
		NodeID: 1, Interface: "vnet0", DurationSec: 10,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("发起失败: %v", err)
	}
	var t1 model.Task
	db.First(&t1, tk.ID)
	if err := capture.NewExecutor(db, agent.NewMockClient()).Run(ctx, &t1); err != nil {
		t.Fatalf("抓包失败: %v", err)
	}

	// 文件已生成 → 删除必须下发到节点。
	delTask, err := svc.Delete(ctx, view.ID, admin(), "root", "")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if delTask == nil {
		t.Fatal("文件已生成时必须下发删除——只删记录会让文件长期留在宿主机上")
	}
	if err := capture.NewDeleteExecutor(db, agent.NewMockClient()).Run(ctx, delTask); err != nil {
		t.Fatalf("执行删除失败: %v", err)
	}

	var n int64
	db.Model(&model.NetworkCapture{}).Where("id = ?", view.ID).Count(&n)
	if n != 0 {
		t.Error("节点确认删除后应清掉控制面记录")
	}
}

// TestDeleteBeforeFileSkippedNode 覆盖文件还没生成时的删除。
//
// 这时不必让节点去删——记录直接删掉即可。
func TestDeleteBeforeFileSkippedNode(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	view, _, err := svc.Start(ctx, capture.Request{
		NodeID: 1, Interface: "vnet0", DurationSec: 30,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("发起失败: %v", err)
	}

	t2, err := svc.Delete(ctx, view.ID, admin(), "root", "")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if t2 != nil {
		t.Error("文件还没生成时不该下发节点删除")
	}
	var n int64
	db.Model(&model.NetworkCapture{}).Count(&n)
	if n != 0 {
		t.Error("记录应被直接删掉")
	}
}

func TestNodeMissingRejected(t *testing.T) {
	_, svc := newEnv(t)

	_, _, err := svc.Start(context.Background(), capture.Request{
		NodeID: 999, Interface: "vnet0", DurationSec: 30,
	}, admin(), "root", "")
	assertStatus(t, err, 404)
}
