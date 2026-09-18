package model

import "time"

// CPUAffinityPreset 对应 cpu_affinity_preset 表：CPU 亲和性预设。
//
// 为什么要"预设"而不是每次手填 cpuset：绑定物理核是**为性能做的一件事**
// （避免虚拟机在核之间漂移、或让同一台机器的核与它的 NUMA 节点对上），
// 而它的写法（`0-3,8`）对用户不友好且**写错了不报错**——只表现为"性能
// 不如预期"。存成有名字的预设之后，常用组合只需理解一次。
type CPUAffinityPreset struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"column:node_id;not null;uniqueIndex:uniq_cpu_affinity_node_name,priority:1,where:deleted_at IS NULL"`

	// Name 在同一节点内唯一（软删除的行不占用名字——删掉「高性能」之后
	// 应当能重建同名的，而不会撞上一句无法理解的唯一约束冲突）。
	Name string `gorm:"size:64;not null;uniqueIndex:uniq_cpu_affinity_node_name,priority:2,where:deleted_at IS NULL"`

	// CPUSet 是绑定的物理核，形如 `0-3` 或 `0,2,4`。
	//
	// **它会被拼进 libvirt 的域配置**，因此只允许数字、逗号与连字符：
	// 少一种可表达的写法，就少一整类绕过方式（与目录共享只收相对路径同理）。
	CPUSet string `gorm:"column:cpuset;size:128;not null"`

	Remark *string `gorm:"size:255"`

	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time `gorm:"index"`
}

// TableName 固定表名。
func (CPUAffinityPreset) TableName() string { return "cpu_affinity_preset" }
