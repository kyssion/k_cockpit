package authz_test

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
)

// TestViewerOfFailsClosed 覆盖视角构造的两个分支。
//
// 未认证时返回零值视角（IsAdmin=false、UserID=0）是**安全失败方向**：
// 归属过滤会变成 `owner_id = 0`，一条都查不到。反过来若返回 IsAdmin=true，
// 一个没登录的请求就能看到全部数据。
func TestViewerOfFailsClosed(t *testing.T) {
	var got authz.Viewer

	h := build()
	h.GET("/viewer", withUser(nil), func(_ context.Context, c *app.RequestContext) {
		got = authz.ViewerOf(c)
		c.String(consts.StatusOK, "ok")
	})
	ut.PerformRequest(h.Engine, "GET", "/viewer", nil)
	if got.IsAdmin || got.UserID != 0 {
		t.Errorf("未认证视角 = %+v, 期望零值（fail-closed）", got)
	}

	h2 := build()
	h2.GET("/viewer", withUser(&model.User{ID: 42, Role: model.RoleAdmin}),
		func(_ context.Context, c *app.RequestContext) {
			got = authz.ViewerOf(c)
			c.String(consts.StatusOK, "ok")
		})
	ut.PerformRequest(h2.Engine, "GET", "/viewer", nil)
	if !got.IsAdmin || got.UserID != 42 {
		t.Errorf("管理员视角 = %+v, 期望 {42 true}", got)
	}

	h3 := build()
	h3.GET("/viewer", withUser(&model.User{ID: 7, Role: model.RoleTenant}),
		func(_ context.Context, c *app.RequestContext) {
			got = authz.ViewerOf(c)
			c.String(consts.StatusOK, "ok")
		})
	ut.PerformRequest(h3.Engine, "GET", "/viewer", nil)
	if got.IsAdmin || got.UserID != 7 {
		t.Errorf("租户视角 = %+v, 期望 {7 false}", got)
	}
}

// ctxKeyUser 是 auth 包注入当前用户用的键。
//
// 它没有导出（ctxKeyUser 是 auth 的私有常量），这里用字面量复现，是为了
// 让被测的中间件在**没有** auth 依赖的情况下也能拿到用户——authz 只依赖
// `auth.CurrentUser(c)` 这一个读接口，测试不必拉起整套登录链路。
// 若哪天 auth 改了键名，这条测试会以「401 而不是 200」失败，那正是想要
// 的提醒：两处对同一个键的约定必须一起改。
const ctxKeyUser = "current_user"

// withUser 模拟认证中间件：把用户放进上下文后继续。
func withUser(u *model.User) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if u != nil {
			c.Set(ctxKeyUser, u)
		}
		c.Next(ctx)
	}
}

// build 组装一条「鉴权 → 业务」的最小链路。
func build() *server.Hertz {
	return server.Default(server.WithDisablePrintRoute(true))
}

// TestRequireRoleAllowsMatchingRole 覆盖放行。
//
// 多角色声明时**任一命中即可**：管理员与运维都要能进同一个接口，写成
// 全都要满足会让这类接口只能由一个人用。
func TestRequireRoleAllowsMatchingRole(t *testing.T) {
	for _, role := range []string{model.RoleAdmin, model.RoleTenant} {
		h := build()
		h.GET("/probe",
			withUser(&model.User{ID: 1, Role: role}),
			authz.RequireRole(model.RoleAdmin, model.RoleTenant),
			func(_ context.Context, c *app.RequestContext) { c.String(consts.StatusOK, "ok") },
		)

		w := ut.PerformRequest(h.Engine, "GET", "/probe", nil)
		if w.Code != consts.StatusOK {
			t.Errorf("角色 %q 状态码 = %d, 期望 200 (body=%s)", role, w.Code, w.Body.String())
		}
	}
}

// TestRequireRoleRejectsOtherRole 覆盖不匹配。
//
// 403 而不是 404：这里的资源是「接口」而不是「某个对象」，隐藏它的存在
// 没有意义，反而让排障时看不出是权限问题。
func TestRequireRoleRejectsOtherRole(t *testing.T) {
	h := build()
	h.GET("/probe",
		withUser(&model.User{ID: 2, Role: model.RoleTenant}),
		authz.Admin(),
		func(_ context.Context, c *app.RequestContext) { c.String(consts.StatusOK, "should not reach") },
	)

	w := ut.PerformRequest(h.Engine, "GET", "/probe", nil)
	if w.Code != consts.StatusForbidden {
		t.Fatalf("状态码 = %d, 期望 403 (body=%s)", w.Code, w.Body.String())
	}
}

// TestRequireRoleWithoutUserIsUnauthenticated 覆盖「没登录」与「权限不够」的区分。
//
// 401 与 403 的处理方式完全不同：前者去登录，后者去找管理员要权限。
// 把它们混成一个码，用户会反复登录却始终进不去。
//
// 这条分支看起来不可达（中间件顺序固定是认证在前），但**顺序恰恰是最容易
// 被改错的地方**——把 RequireRole 挂到 requireAuth 前面，或者某条路由单独
// 注册时漏了认证，都会落到这里。一个 401 比一个 500 或 panic 好得多。
func TestRequireRoleWithoutUserIsUnauthenticated(t *testing.T) {
	h := build()
	h.GET("/probe",
		withUser(nil),
		authz.Admin(),
		func(_ context.Context, c *app.RequestContext) { c.String(consts.StatusOK, "should not reach") },
	)

	w := ut.PerformRequest(h.Engine, "GET", "/probe", nil)
	if w.Code != consts.StatusUnauthorized {
		t.Fatalf("状态码 = %d, 期望 401 (body=%s)", w.Code, w.Body.String())
	}
}

// TestRequireRoleAbortsChain 覆盖「拒绝后不再执行后续 handler」。
//
// 只断言状态码是不够的：如果漏了 c.Abort()，业务 handler 仍会执行并写库，
// 而响应已经被 403 占位——结果是一次**看不见的越权写入**。
func TestRequireRoleAbortsChain(t *testing.T) {
	reached := false
	h := build()
	h.GET("/probe",
		withUser(&model.User{ID: 3, Role: model.RoleTenant}),
		authz.Admin(),
		func(_ context.Context, c *app.RequestContext) {
			reached = true
			c.String(consts.StatusOK, "leaked")
		},
	)

	w := ut.PerformRequest(h.Engine, "GET", "/probe", nil)
	if w.Code != consts.StatusForbidden {
		t.Fatalf("状态码 = %d, 期望 403", w.Code)
	}
	if reached {
		t.Error("被拒绝后仍然执行了业务 handler —— 漏了 c.Abort()，这是一次越权写入")
	}
}
