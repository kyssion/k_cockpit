package model

import "time"

// 调度事件的状态。
const (
	// SchedulerDone 表示这次调度**实际做了事**并成功。
	SchedulerDone = "done"
	// SchedulerFailed 表示这次调度尝试做事但失败了。
	//
	// 失败与成功一样值得记：一个一直失败又一直重试的调度器如果只留下
	// 「没有动作」的空白，用户会以为它工作正常。
	SchedulerFailed = "failed"
)

// SchedulerEvent 对应 scheduler_event 表：周期性调度器**实际发生的动作**（F-7-04）。
//
// 这张表的设计里最重要的一条是**它不该有的东西**：
//
//	**不记录普通轮询。**
//
// 一个每 30 秒醒一次的调度器，一天会醒 2880 次。如果每次都写一条「检查过，
// 无事发生」，那么真正有价值的那一条——"清理了 500 个过期任务"或"有 3 次
// 执行失败"——会淹没在两千多条噪声里，而用户翻事件列表的目的恰恰是找它。
//
// 因此：**有动作才写，没有动作什么都不留。**
//
// 由此推出一个容易被误读的地方：**某调度器没有事件不等于它没在运行**。
// 界面上必须把它正在跑、只是最近没做事这件事说出来（见 scheduler 包的
// 注册表），否则「事件为空」会被读成「这个东西坏了」。
type SchedulerEvent struct {
	ID int64 `gorm:"primaryKey"`

	// SchedulerKey 是调度器的稳定标识（代码里写死的常量）。
	//
	// 与 SchedulerName 分开：名字是给人看的、可能改，而 key 要能跨版本
	// 对上——按名字关联的话，某天把「指标采集」改成「监控采集」，历史事件
	// 就全部对不上了。
	SchedulerKey string `gorm:"column:scheduler_key;size:64;not null"`
	// SchedulerName 是事件发生时的名字，**冗余存一份**。
	//
	// 不留成关联查询：调度器是代码里的常量而不是表里的行，名字改了之后
	// 历史事件应当保留当时那个名字。「当时它叫什么」是历史的一部分。
	SchedulerName *string `gorm:"column:scheduler_name;size:64"`
	// GroupName 是分组（指标 / 任务 / 定时 / 维护）。
	GroupName *string `gorm:"column:group_name;size:64"`

	NodeID *int64 `gorm:"column:node_id"`
	// Scope 是这次动作影响的范围（节点名、虚拟机名、或一句概括）。
	//
	// 它回答「动了谁」——没有它的话，「清理了 500 个任务」这句话对排查
	// 毫无帮助，而「清理了 500 个任务（节点 node-3）」至少能定位。
	Scope *string `gorm:"size:128"`

	Status string `gorm:"size:16;not null"`
	// Message 是给人看的一句话，写清**做了什么**而不只是"成功"。
	Message *string `gorm:"size:512"`

	// At 是动作发生的时刻。
	//
	// 不加 `default:now()`：迁移里的默认值是真的（数据库侧兜底），但
	// 模型上写它会让 AutoMigrate 在 SQLite 下建不出表（`now()` 不是
	// SQLite 的默认值写法），而测试库是按模型建的。时间统一由 Go 侧
	// 赋值——与 host_stats_record 等表一致。
	At        time.Time `gorm:"not null;index:idx_scheduler_event_at"`
	CreatedAt time.Time
}

// TableName 固定表名。
func (SchedulerEvent) TableName() string { return "scheduler_event" }

// ValidSchedulerStatus 报告状态取值是否合法。
func ValidSchedulerStatus(s string) bool {
	switch s {
	case SchedulerDone, SchedulerFailed:
		return true
	}
	return false
}
