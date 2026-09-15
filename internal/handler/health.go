// Package handler 实现 HTTP 接口。
//
// handler 只负责协议层工作：解析请求、调用下游、组织响应，
// 不承载复杂业务逻辑。
package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
)

// healthStatus 是健康检查的返回数据。
type healthStatus struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

// Health 返回服务与数据库的健康状态，供探活与就绪检查使用。
//
// 无需认证（API.md 中显式标记的公开接口）。
func Health(db *gorm.DB) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.PingContext(ctx) != nil {
			api.Fail(c, api.Unavailable("数据库不可用"))
			return
		}

		api.OK(c, healthStatus{Status: "ok", Database: "up"})
	}
}
