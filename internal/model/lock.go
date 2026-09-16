package model

import "time"

// VMLock 对应 vm_lock 表：虚拟机的**业务软锁**（F-2-12）。
//
// 它不是虚拟化层的锁，用户也解不开——它只约束本面板的行为：锁定期间禁止
// 删除、禁止磁盘迁移。存在的理由是一类具体事故：一台长期运行的机器，某天
// 在列表里被顺手删掉（看错行、选错项、以为那是另一台测试机），而连盘删除
// 是不可逆的。加锁让这一步必须先解开一道明确的锁，而不是在同一个页面里
// 顺手完成。
//
// 表上有 uniq_vm_lock_vm_id 唯一索引：一台虚拟机最多一行，加锁/解锁都是
// 对同一行的更新，不会累积历史（历史由审计负责记录，那是它该待的地方）。
type VMLock struct {
	ID int64 `gorm:"primaryKey"`
	// VMID 对应虚拟机 id，**唯一**。
	VMID int64 `gorm:"not null;uniqueIndex:uniq_vm_lock_vm_id"`

	// Locked 为 false 时这一行只是「曾经锁过」的痕迹，不产生任何约束。
	Locked bool `gorm:"not null;default:false"`

	// LockedBy 是加锁人。解锁时清空——保留它会让人误以为「现在还是他锁着」。
	LockedBy *int64

	// Reason 是加锁原因（可选）。界面上要显示它：一句「已锁定」不告诉用户
	// 该找谁、为什么不能删，他只会去猜或者干脆放弃。
	Reason *string `gorm:"size:255"`

	// LockedAt 是加锁时间。解锁时置空，与 Locked 同进同退——
	// 一个「未锁定但有加锁时间」的状态在界面上无法解释。
	LockedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMLock) TableName() string { return "vm_lock" }

// IsLocked 报告这一行是否构成有效锁定。
//
// 允许接收 nil，便于调用方在「查不到记录」时直接判断——查不到就是没锁，
// 让调用方写 `if lock != nil && lock.Locked` 容易漏掉一半。
func (l *VMLock) IsLocked() bool {
	return l != nil && l.Locked
}
