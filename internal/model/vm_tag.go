package model

import "time"

// VMTag 对应 vm_tag 表：虚拟机上的一个标签（F-2-16）。
//
// 标签与分组（`vm.group_name`）解决的不是同一件事，两者都需要：
//
//	**分组**是**互斥**的——一台机器属于且只属于一个组，它回答"这台机器
//	在哪一批里"；**标签**是**非互斥**的——一台机器可以有任意多个，它回答
//	"这台机器有哪些属性"。
//
// 把两者合并成一个字段会立刻遇到矛盾：一台机器既是"生产"又是"数据库"，
// 而分组只能放一个。反过来，如果全靠标签，就失去了"这批机器一共几台"
// 这种可以求和的视图。
type VMTag struct {
	ID   int64 `gorm:"primaryKey"`
	VMID int64 `gorm:"not null;uniqueIndex:uniq_vm_tag_vm_tag,priority:1"`
	// Tag 长度上限 32：标签是给人扫读的，不是给人写描述的。
	//
	// 一个 200 字的标签在列表里显示不下、只能截断，而截断之后的标签
	// 反而分不清——不如逼着写短的。
	Tag string `gorm:"size:32;not null;uniqueIndex:uniq_vm_tag_vm_tag,priority:2;index:idx_vm_tag_tag"`

	CreatedAt time.Time
}

// TableName 固定表名。
func (VMTag) TableName() string { return "vm_tag" }

// MaxTagLen 是标签长度上限。
const MaxTagLen = 32
