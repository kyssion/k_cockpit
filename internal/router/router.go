// Package router 集中注册 HTTP 路由。
//
// 所有对外暴露的接口都在这里登记，便于一眼看清服务的 API 面。
// 当前只有健康检查；业务接口随功能实现逐步接入，并在
// docs/03-api/API.md 的接口清单中登记。
package router

import (
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"k_cockpit/internal/handler"
)

// Register 注册全部路由。
func Register(h *server.Hertz, db *gorm.DB) {
	h.GET("/health", handler.Health(db))
}
