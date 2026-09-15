// Package router 集中注册 HTTP 路由。
//
// 所有对外暴露的接口都在这里登记，便于一眼看清服务的 API 面。
// 业务接口随功能实现逐步接入，并在 docs/03-api/API.md 的接口清单中登记。
package router

import (
	"github.com/cloudwego/hertz/pkg/app/server"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/handler"
)

// Register 注册全局中间件与全部路由。
func Register(h *server.Hertz, db *gorm.DB) {
	// 顺序：RequestID 最先（后续都要用它）；Recover 包裹业务处理，
	// 保证 panic 也被记录 request_id；AccessLog 在最内层以准确统计耗时。
	h.Use(api.RequestID(), api.Recover(), api.AccessLog())

	h.GET("/health", handler.Health(db))
}
