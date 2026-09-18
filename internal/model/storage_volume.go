package model

import (
	"strings"
	"time"
)

// 存储卷的类型。
const (
	// VolumeKindLVM 是 LVM 逻辑卷，**当前唯一支持的一种**。
	//
	// 保留这个字段而不是直接写死：卷的形态决定了它的聚合方式与操作命令，
	// 而将来如果接入 ZFS 或 Btrfs，这个字段是唯一的区分点。现在只有一个
	// 取值不影响什么，但少了它，将来那次扩展要动的是表结构。
	VolumeKindLVM = "lvm"
)

// 卷的状态。
const (
	// VolumeActive 表示卷可用。
	VolumeActive = "active"
	// VolumeSync 表示**镜像正在初始同步**。
	//
	// 它与 active 的区别是真实的、也是用户能感觉到的：镜像卷建好之后，
	// LVM 要把已有数据复制到第二份上，这段时间里读正常、**写会明显变慢**
	// （有时慢几倍），而同步可能持续几小时。不标出来的话，用户会以为卷
	// 出了故障，或者以为是自己选错了参数。
	VolumeSync = "sync"
	// VolumeDegraded 表示镜像少了一份。
	//
	// 卷还能用，但**已经没有冗余了**——此时再坏一块盘就全丢。这是最需要
	// 让用户看到的一种状态，因为它看起来一切正常。
	VolumeDegraded = "degraded"
	// VolumeFailed 表示设备出错或聚合已损坏。
	VolumeFailed = "failed"
)

// StorageVolume 对应 storage_volume 表：由多块物理盘聚合出的存储卷（F-5-02）。
//
// 这一块最需要讲清楚的不是怎么建，而是**一个几乎所有人都会有的误解**：
//
//	**条带 ≠ 冗余。**
//
// 把数据分散到多块盘上（条带）确实能提升吞吐，但它**不提供任何保护**——
// 任何一块盘故障都会让整个卷不可用，而且因为数据是分散的，剩下那些盘上的
// 内容也无法单独恢复。这与「多块盘放在一起」给人的直觉正好相反。
//
// 换句话说：**条带是拿可靠性换性能，镜像是拿容量换可靠性。** 两者方向
// 相反，而界面上必须把它们说得同样清楚——用户看到「用了 4 块盘」时，
// 很自然会以为那是"4 块盘的冗余"。
type StorageVolume struct {
	ID     int64 `gorm:"primaryKey"`
	NodeID int64 `gorm:"not null;index:idx_storage_volume_node_id;uniqueIndex:uniq_storage_volume_node_name,priority:1"`

	Name string `gorm:"size:64;not null;uniqueIndex:uniq_storage_volume_node_name,priority:2"`

	Kind string `gorm:"size:16;not null;default:lvm"`

	// VGName / LVName 是 LVM 上的实际名称。
	//
	// 与上面的 Name（给人看的）分开：LVM 对卷组与逻辑卷名有字符集限制，
	// 用户取的中文名或带空格的名字必须映射成一个合法名称。把它们混为一谈
	// 会让「名字里有个空格就建不出来」，而报错来自 LVM、与空格毫无关系。
	VGName *string `gorm:"column:vg_name;size:64"`
	LVName *string `gorm:"column:lv_name;size:64"`

	// SizeGB 是卷的**可用容量**。
	//
	// 注意它与物理占用不是一回事：镜像卷会占用 SizeGB × MirrorCount 的
	// 实际空间，而界面上如果只显示 SizeGB，用户会以为「还有那么多空间」。
	SizeGB int `gorm:"not null;default:0"`

	// StripeCount 是条带数（数据分散到几块盘上）。
	//
	// 为 0 或 1 表示不使用条带。
	StripeCount int `gorm:"not null;default:0"`
	// MirrorCount 是镜像份数。
	//
	// 为 0 或 1 表示不做镜像。「它是这块表上唯一影响能不能坏一块盘的东西」，而 StripeCount 完全不影响——这一点在界面上要写清楚。
	MirrorCount int `gorm:"not null;default:0"`

	// Devices 是参与聚合的物理设备列表（换行分隔）。
	//
	// 存列表而不是关联表：设备的组成**只在整卷创建时确定一次**，没有
	// 「单独给这个卷加一块盘」这种操作（那需要重构整个聚合）。因此不需要
	// 行级的增删。
	Devices *string `gorm:"type:text"`

	Status string `gorm:"size:16;not null;default:active"`

	// **没有 remark，也没有 deleted_at** —— 表里就是没有这两列。
	//
	// 失败原因不落在这里：一条创建失败的卷本来就不该存在（它的设备没被
	// 占用、盘上也没有数据），留一条「失败」记录只会让用户卡在「同名卷
	// 已存在」上。原因由任务承担，那里才是排查的入口——与目录共享同一套
	// 处理。
	//
	// 删除也不留软删除记录：删除是用户的明确意图（销毁数据），删掉记录
	// 与事实一致，而且**保留记录会让那些设备一直显示为被占用**，用户再也
	// 建不了新卷。至于「谁在什么时候删了哪个卷、销毁了多少数据」，由审计
	// 流水回答——那里的 resource_id 与 resource_name 足够。
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (StorageVolume) TableName() string { return "storage_volume" }

// DeviceList 返回参与聚合的设备。
func (v *StorageVolume) DeviceList() []string {
	if v.Devices == nil || strings.TrimSpace(*v.Devices) == "" {
		return nil
	}
	fields := strings.FieldsFunc(*v.Devices, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// PhysicalGB 返回该卷实际占用的物理空间。
//
// **它不是 SizeGB**：镜像会把每一份数据写多遍，因此占用是可用容量的
// MirrorCount 倍。两个数字都要给用户看到——只看可用容量会让人以为
// 「还能再建一个这么大的卷」，而物理盘早就满了。
func (v *StorageVolume) PhysicalGB() int {
	mult := v.MirrorCount
	if mult < 1 {
		mult = 1
	}
	return v.SizeGB * mult
}

// DeviceCount 返回聚合所需的**最少**设备数。
//
// 条带与镜像会相乘：做 2 路镜像、每路 2 条带，就需要 4 块盘。
//
// 这个乘法是「直觉容易出错」的地方——用户会想「镜像要 2 块、条带要 2 块，
// 那我给 2 块盘就行了吧」。实际上每一份镜像自己也要分散到多条带上，因此
// 需要 stripe × mirror 块。
func (v *StorageVolume) DeviceCount() int {
	stripe := v.StripeCount
	if stripe < 1 {
		stripe = 1
	}
	mirror := v.MirrorCount
	if mirror < 1 {
		mirror = 1
	}
	return stripe * mirror
}

// HasRedundancy 报告该卷能否容忍一块盘故障。
//
// **只有镜像能**。条带完全不能——这个判断是界面上那句提示的依据，
// 也是把「有没有冗余」这件事从一段文案变成一个可计算的事实。
func (v *StorageVolume) HasRedundancy() bool { return v.MirrorCount > 1 }

// ValidVolumeStatus 报告状态取值是否合法。
func ValidVolumeStatus(s string) bool {
	switch s {
	case VolumeActive, VolumeSync, VolumeDegraded, VolumeFailed:
		return true
	}
	return false
}
