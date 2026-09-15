// Package authz 实现授权：角色判定与资源归属过滤（f-1-06）。
//
// 两条设计约束：
//   - **角色要求通过路由注册时声明**（R-003），而不是在 handler 内部判断——
//     这样「哪些接口需要什么角色」可被集中审查（见 internal/router）；
//   - 前端隐藏菜单**不构成安全边界**（R-010）：它只决定看不看得到。
package authz

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
)

// RequireRole 要求当前用户具备指定角色之一。
//
// 必须置于认证中间件**之后**：它依赖已注入上下文的当前用户。
func RequireRole(roles ...string) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		user := auth.CurrentUser(c)
		if user == nil {
			// 未认证时返回 401 而非 403：后者会让人以为「权限不够」，
			// 而真实原因是「还没登录」——两者的处理方式完全不同。
			api.Fail(c, api.Unauthenticated("请先登录"))
			c.Abort()
			return
		}

		for _, role := range roles {
			if user.Role == role {
				c.Next(ctx)
				return
			}
		}

		api.Fail(c, api.PermissionDenied("当前角色无权访问该接口"))
		c.Abort()
	}
}

// Admin 是 RequireRole(model.RoleAdmin) 的简写。
func Admin() app.HandlerFunc {
	return RequireRole("admin")
}
