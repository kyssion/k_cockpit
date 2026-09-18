package model

import "time"

// NetworkCapture 对应 network_capture 表：一次限时抓包（F-4-12）。
//
// 表上的两个字段各自承载一条**必须存在**的约束，缺任何一个都会出事：
//
//	DurationSec  抓包**必须有时限**
//	ExpiresAt    抓包**文件必须过期消失**
//
// 前者：不限时的抓包会把宿主机磁盘写满，而且用户会忘记停——它的失败不是
// "没抓到"，而是"把宿主机写挂了"，而那时它已经跑了几小时。
//
// 后者：抓包文件里有**完整的流量内容**，包括明文密码、会话令牌、内网数据。
// 它不是一份普通的日志，而是一份"这段时间这个网口上发生的一切"。留在
// 宿主机上越久，泄露面越大，而它多半是在排查完之后就被忘掉的。
type NetworkCapture struct {
	ID     int64  `gorm:"primaryKey"`
	NodeID int64  `gorm:"column:node_id;not null;index:idx_network_capture_node_id"`
	VMID   *int64 `gorm:"column:vm_id"`

	// Interface 是要抓的网口。
	Interface *string `gorm:"size:64"`
	// Filter 是 BPF 过滤表达式（如 `tcp port 80`）。
	//
	// **它是一处注入面**：表达式最终会被拼进 tcpdump 的命令行。因此校验
	// 必须发生在控制面（见 capture 包），而不只是"节点侧会小心处理"——
	// 节点侧的转义是第二道防线，不是第一道。
	Filter *string `gorm:"size:255"`

	// DurationSec 是抓包时长（秒）。
	DurationSec int `gorm:"column:duration_sec;not null;default:0"`

	FilePath  *string `gorm:"column:file_path;size:512"`
	SizeBytes int64   `gorm:"column:size_bytes;not null;default:0"`
	TaskID    *int64  `gorm:"column:task_id;index:idx_network_capture_task_id"`
	CreatedBy *int64  `gorm:"column:created_by"`

	// ExpiresAt 是文件在节点上保留到什么时候。
	//
	// 到期由节点侧的清理负责（文件在它那里），控制面这一列是**记录**：
	// 它让用户能看到"这份抓包还能下载多久"，也让过期之后的删除有据可依。
	ExpiresAt *time.Time `gorm:"column:expires_at"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (NetworkCapture) TableName() string { return "network_capture" }

// Ready 报告抓包文件是否已经生成。
//
// 抓包是**异步**的：下发之后要等 duration 秒才有文件。这期间界面必须
// 显示"进行中"而不是"文件不存在"——后者会让用户以为抓包失败了。
func (c *NetworkCapture) Ready() bool { return c.FilePath != nil && *c.FilePath != "" }

// Expired 报告文件是否已过期（按给定的时刻判断）。
func (c *NetworkCapture) Expired(now time.Time) bool {
	return c.ExpiresAt != nil && now.After(*c.ExpiresAt)
}
