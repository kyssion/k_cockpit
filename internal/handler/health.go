// Package handler 实现 HTTP 接口。
//
// handler 只负责协议层工作：解析请求、调用下游、组织响应，
// 不承载复杂业务逻辑。
package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/utils"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"
)

// Health 返回服务与数据库的健康状态，供探活与就绪检查使用。
func Health(db *gorm.DB) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.PingContext(ctx) != nil {
			c.JSON(consts.StatusServiceUnavailable, utils.H{
				"status":   "unhealthy",
				"database": "down",
			})
			return
		}

		c.JSON(consts.StatusOK, utils.H{
			"status":   "ok",
			"database": "up",
		})
	}
}
