package model

import "time"

// 迁移状态。与 task.status 用同一套词汇。
const (
	MigrationPending = "pending"
	MigrationRunning = "running"
	MigrationSuccess = "success"
	MigrationFailed  = "failed"
)

// VMMigration 对应 vm_migration 表：一次跨节点迁移（F-2-09）。
//
// 它记录的是**过程**而不是结果：`vm.node_id` 说明「现在在哪」，本表说明
// 「从哪来、什么时候、成没成」。迁移出问题时要回答的往往是后者——例如
// 「这台机器的磁盘是不是还留在原宿主机上」。
type VMMigration struct {
	ID     int64  `gorm:"primaryKey"`
	VMID   int64  `gorm:"not null;index:idx_vm_migration_vm_id"`
	VMName string `gorm:"size:128;not null"`

	FromNodeID int64 `gorm:"not null"`
	ToNodeID   int64 `gorm:"not null"`

	Status string `gorm:"size:16;not null;default:pending"`
	// Result 说明跟着搬了些什么（网卡、静态地址、端口转发的数量）。
	//
	// 只给一个「成功」会让用户不确定「我原来接的网络、配的转发还在不在」，
	// 而那是他迁移前最关心的事之一。
	Result    *string `gorm:"size:512"`
	Error     *string `gorm:"size:512"`
	CreatedBy *int64

	CreatedAt  time.Time
	FinishedAt *time.Time
}

// TableName 固定表名。
func (VMMigration) TableName() string { return "vm_migration" }

// IsRunning 报告迁移是否仍在进行中。
func (m *VMMigration) IsRunning() bool {
	return m.Status == MigrationPending || m.Status == MigrationRunning
}
