package ops

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/route/param"
	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	cron "k_cockpit/internal/service/ops/schedule"
)

// 定点：虚拟机的定时任务里，PATCH 只负责启停。
//
// 这一组用例针对的是一类**静默的假成功**：前端把整张表单回传成
// `{enabled, time_of_day, action}`，接口只绑 enabled、其余字段被丢掉，
// 却照样返回 200。用户据此以为执行时间改了，直到某天发现任务还在老时间跑
// ——那时他已经想不起是哪一步没生效。丢掉与被拒之间必须选后者。
const (
	testVMID       int64 = 1
	testScheduleID int64 = 2
)

func newScheduleHandlerEnv(t *testing.T) (*Schedule, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "ops_schedule.db"),
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

	if err := db.AutoMigrate(&model.VM{}, &model.VMSchedule{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.VM{
		ID: testVMID, NodeID: 1, Name: "vm-sched", Status: model.VMStatusRunning,
	}).Error; err != nil {
		t.Fatalf("创建测试虚拟机失败: %v", err)
	}

	runAt := "03:00"
	if err := db.Create(&model.VMSchedule{
		ID: testScheduleID, VMID: testVMID, NodeID: 1,
		Action: model.ScheduleActionStart, ScheduleType: model.ScheduleTypeDaily,
		RunAt: &runAt, Enabled: true,
	}).Error; err != nil {
		t.Fatalf("创建测试定时任务失败: %v", err)
	}

	// risk.Guard 只在 Create 里用到，启停路径不需要它。
	return NewSchedule(cron.NewService(db), nil), db
}

// patchSchedule 直接调用 handler，并把路径参数与用户按中间件的方式注入。
//
// 用 app.NewContext 而不是起一个真实引擎：这些用例要验的是**请求体解析与
// 拒绝规则**，路由层（路径参数名）已有各自的测试覆盖。
func patchSchedule(t *testing.T, h *Schedule, body string) *app.RequestContext {
	t.Helper()

	ctx := app.NewContext(0)
	ctx.Params = append(ctx.Params,
		param.Param{Key: "id", Value: "1"},
		param.Param{Key: "scheduleID", Value: "2"},
	)
	ctx.Request.Header.SetContentTypeBytes([]byte("application/json"))
	ctx.Request.SetBodyString(body)
	// Content-Length 必须一起给：绑定器按它判断「有没有请求体」，缺了会把
	// 一次带 body 的请求当成空的（真实请求里这个头由框架填好）。
	ctx.Request.Header.SetContentLength(len(body))
	// 认证中间件把当前用户放在这个键上，auth.CurrentUser 从同一处读回。
	// 不注入的话视角是「未登录的普通用户」，连虚拟机都查不到（404）。
	ctx.Set("current_user", &model.User{ID: 1, Role: model.RoleAdmin})

	h.SetEnabled(context.Background(), ctx)
	return ctx
}

// errorMessage 从错误响应体里取出 error.message。
func errorMessage(t *testing.T, ctx *app.RequestContext) string {
	t.Helper()

	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(ctx.Response.Body(), &body); err != nil {
		t.Fatalf("解析错误响应失败: %v (body=%s)", err, ctx.Response.Body())
	}
	return body.Error.Message
}

// TestSetEnabledOnlyEnabledSucceeds 覆盖正常路径：只传 enabled → 200。
func TestSetEnabledOnlyEnabledSucceeds(t *testing.T) {
	h, db := newScheduleHandlerEnv(t)

	ctx := patchSchedule(t, h, `{"enabled":false}`)
	if got := ctx.Response.StatusCode(); got != 200 {
		t.Fatalf("状态码 = %d, 期望 200（body=%s）", got, ctx.Response.Body())
	}

	var row model.VMSchedule
	if err := db.Where("id = ?", testScheduleID).First(&row).Error; err != nil {
		t.Fatalf("读取定时任务失败: %v", err)
	}
	if row.Enabled {
		t.Error("enabled=false 应把任务停用")
	}
	// 停用要顺手清掉下次执行时刻，否则调度器的扫描条件会一直命中。
	if row.NextRunAt != nil {
		t.Errorf("停用后 next_run_at 应为空, 实际 %v", row.NextRunAt)
	}
}

// TestSetEnabledRejectsUnsupportedFields 覆盖本缺陷本身。
//
// 带了本接口改不了的字段就拒绝：静默丢弃会让调用方以为改成功了。
func TestSetEnabledRejectsUnsupportedFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		// field 应出现在报错文案里，让调用方知道是哪一项没被接受。
		field string
	}{
		{"执行时间", `{"enabled":true,"time_of_day":"03:00"}`, "time_of_day"},
		{"动作", `{"enabled":true,"action":"shutdown"}`, "action"},
		{"调度类型", `{"enabled":true,"schedule_type":"weekly"}`, "schedule_type"},
		{"cron 表达式", `{"enabled":true,"cron":"0 3 * * *"}`, "cron"},
		{"名称", `{"enabled":true,"name":"改名"}`, "name"},
		// 取值是空的也算「带了」：调用方同样以为自己改了什么。
		{"空值的执行时间", `{"enabled":true,"time_of_day":""}`, "time_of_day"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newScheduleHandlerEnv(t)

			ctx := patchSchedule(t, h, tc.body)
			if got := ctx.Response.StatusCode(); got != 400 {
				t.Fatalf("状态码 = %d, 期望 400（body=%s）", got, ctx.Response.Body())
			}

			msg := errorMessage(t, ctx)
			// 文案要说清「本接口能做什么」以及「想改该去哪儿改」。
			for _, want := range []string{"仅支持启停", tc.field, "删除该任务后重新创建"} {
				if !strings.Contains(msg, want) {
					t.Errorf("文案缺少 %q: %s", want, msg)
				}
			}

			// 被拒绝的请求一个字段都不能改。
			var row model.VMSchedule
			if err := db.Where("id = ?", testScheduleID).First(&row).Error; err != nil {
				t.Fatalf("读取定时任务失败: %v", err)
			}
			if !row.Enabled {
				t.Error("被拒绝的请求竟然把任务停用了")
			}
			if row.Action != model.ScheduleActionStart {
				t.Errorf("被拒绝的请求改动了动作: %q", row.Action)
			}
		})
	}
}

// TestSetEnabledRequiresEnabledField 固定既有行为：缺 enabled 仍是 400。
func TestSetEnabledRequiresEnabledField(t *testing.T) {
	h, db := newScheduleHandlerEnv(t)

	ctx := patchSchedule(t, h, `{}`)
	if got := ctx.Response.StatusCode(); got != 400 {
		t.Fatalf("状态码 = %d, 期望 400（body=%s）", got, ctx.Response.Body())
	}
	if msg := errorMessage(t, ctx); !strings.Contains(msg, "缺少 enabled 字段") {
		t.Errorf("文案 = %q, 期望说明缺少 enabled 字段", msg)
	}

	// 库里不受影响。
	var row model.VMSchedule
	if err := db.Where("id = ?", testScheduleID).First(&row).Error; err != nil {
		t.Fatalf("读取定时任务失败: %v", err)
	}
	if !row.Enabled {
		t.Error("参数缺失不该改动任务状态")
	}
}
