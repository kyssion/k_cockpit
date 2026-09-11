// Package router 集中注册 HTTP 路由。
//
// 所有对外暴露的接口都在这里登记，便于一眼看清服务的 API 面。
package router

import (
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"k_cockpit/internal/handler"
)

// Register 注册全部路由。
func Register(h *server.Hertz, db *gorm.DB) {
	h.GET("/health", handler.Health(db))

	api := h.Group("/api/v1")
	{
		users := handler.NewUser(db)
		api.POST("/users", users.Create)
		api.GET("/users", users.List)
		api.GET("/users/:id", users.Get)
	}
}
