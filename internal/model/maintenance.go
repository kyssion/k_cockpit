package model

import "time"

// SiteMaintenance 是站点级维护状态的唯一一行（id = 1，迁移 0048 铺底）。
//
// 它只描述「站点是否处于维护、由谁发起、接管了哪些节点」；真正的维护
// 状态仍在 node 表上——VM 的创建与电源操作走 ensureNodeUsable 查节点，
// 站点维护因此不需要在业务路径上加第二处判断。
type SiteMaintenance struct {
	ID            int64 `gorm:"primaryKey"`
	InMaintenance bool
	Reason        *string
	ShutdownVMs   bool
	EnteredBy     *int64
	EnteredByName string
	EnteredAt     *time.Time
	// NodeIDs 是本次由站点模式接管维护的节点（逗号分隔）。退出时只清
	// 这些，不碰管理员手工设置的维护。
	NodeIDs string
}

// TableName 使用单数表名（与全项目约定一致，GORM 已配置 SingularTable）。
func (SiteMaintenance) TableName() string { return "site_maintenance" }
