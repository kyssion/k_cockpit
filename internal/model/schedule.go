package model

import "time"

// 定时任务的动作。
//
// 三种动作都**复用已有的任务类型**（vm.power / vm.delete），不需要任何新的
// agent 能力：定时任务做的事只是「到点了替用户点一次按钮」。
const (
	ScheduleActionStart    = "start"
	ScheduleActionShutdown = "shutdown"
	ScheduleActionDelete   = "delete"
	// ScheduleActionSnapshot 定时创建快照。
	//
	// 它与前三种一样**复用已有的任务类型**（vm.snapshot.create），不需要新的
	// agent 能力。它与"删除"的关键差别是可逆：快照可以再删，因此可以周期
	// 执行——而删除只能一次性（见 schedule.Service.Create 里的说明）。
	ScheduleActionSnapshot = "snapshot"
)

// 调度类型。
const (
	ScheduleTypeOnce   = "once"
	ScheduleTypeDaily  = "daily"
	ScheduleTypeWeekly = "weekly"
)

// VMSchedule 对应 vm_schedule 表：一台虚拟机的定时操作。
//
// 关于 LastResult 的取值：`success` / `failed` / `skipped`。**`skipped` 是
// 刻意保留的一类**——服务停机期间错过的时间点不会被补执行（见 scheduler.go
// 的说明），那些点必须留下痕迹，否则用户会以为「任务没跑过」而反复重建。
type VMSchedule struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"not null;index:idx_vm_schedule_vm_id"`
	NodeID int64 `gorm:"not null"`

	Action       string `gorm:"size:16;not null"`
	ScheduleType string `gorm:"size:16;not null"`

	// Weekdays 是每周模式下的星期（1=周一 … 7=周日），逗号分隔如 `1,3,5`。
	// 其它模式下为空。
	Weekdays *string `gorm:"size:32"`

	// RunAt 是执行时刻，形如 `03:00`（PostgreSQL 的 time 类型，无日期）。
	//
	// 用字符串而不是 time.Time：这个列只有「几点几分」的含义，日期部分
	// 由 NextRunAt 承载。用 time.Time 会让它带一个无意义的日期，
	// 而那个日期在读写与比较时都可能造成误判。
	RunAt *string `gorm:"type:time"`

	// NextRunAt 是下次执行时刻（完整时间戳）。为空表示不会再执行。
	//
	// 调度器按 (next_run_at, enabled) 建索引扫描，因此它同时是**扫描键**：
	// 把它与 enabled 一起放进索引，是为了让「找出所有该跑的任务」这一步
	// 不需要回表筛掉已停用的行。
	NextRunAt *time.Time `gorm:"index:idx_vm_schedule_next_run,priority:1"`

	LastRunAt  *time.Time
	LastResult *string `gorm:"size:32"`
	// LastTaskID 指向该次执行产生的任务，便于从定时任务跳到任务详情排查。
	LastTaskID *int64

	// --- 仅 snapshot 动作使用 ---

	// SnapshotName 是快照名模板。留空时由服务端按时间生成一个。
	SnapshotName *string `gorm:"size:128"`
	// IncludeMemory 表示是否保存运行现场。
	IncludeMemory bool `gorm:"not null;default:false"`

	// Enabled 控制任务是否参与调度。
	//
	// ⚠️ **GORM 陷阱**：字段带 `default:true` 时，值为 `false` 会被当作零值
	// **省略不写**，数据库随即填入默认值 `true`——也就是说
	// `Create(&VMSchedule{Enabled: false})` 存进去的是一条**启用**的记录。
	//
	// 因此停用只能走 Update 而不能在建记录时一次完成（见 schedule.Service）。
	// 生产代码里创建时总是 true，不受影响；但改动这里之前请先确认这一点。
	Enabled bool `gorm:"not null;default:true;index:idx_vm_schedule_next_run,priority:2"`

	CreatedBy *int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (VMSchedule) TableName() string { return "vm_schedule" }

// RequiresVerification 报告该动作是否属于高风险操作。
//
// 删除会连同磁盘数据一起消失，与手动删除走同一道二次验证；开机与关机
// 不改变数据，不需要。
func (s *VMSchedule) RequiresVerification() bool {
	return s.Action == ScheduleActionDelete
}

// IsOneTime 报告是否是一次性任务。
func (s *VMSchedule) IsOneTime() bool {
	return s.ScheduleType == ScheduleTypeOnce
}
