package model

import "time"

// 9p VirtFS 的安全模型（f-5-06）。
const (
	// ShareSecurityMapped 把来宾的文件操作**映射到运行虚拟机的宿主机用户**上。
	//
	// 来宾里的 root 写出来的文件，在宿主机上归那个运行用户所有，而不是
	// 归 root。代价是每次文件操作多一层映射（大量小文件时会慢），换来的是
	// 来宾**无法伪造宿主机上的文件所有者**。
	ShareSecurityMapped = "mapped"
	// ShareSecurityPassthrough 直接用来宾的 uid/gid。
	//
	// 更快，但意味着来宾里的 root 可以写出宿主机上属于 root 的文件。共享
	// 目录与宿主机之间因此不再有权限边界——只在**完全受信的来宾**上才成立。
	ShareSecurityPassthrough = "passthrough"
	// ShareSecurityNone 不做任何权限检查。
	ShareSecurityNone = "none"
)

// ShareMount 对应 share_mount 表：把宿主机上的一个目录以 9p VirtFS 挂进
// 虚拟机（F-5-06）。
//
// **host_path 是这台机器上最需要小心的一处输入**。
//
// 它是一个**跨越虚拟化边界的读取入口**，而它的参数来自用户：9p 共享让来宾
// 能读（乃至写）宿主机上的一个目录，如果允许指定任意路径，一个租户可以把
// 别人的数据目录、甚至 /etc 挂进自己的虚拟机读出来——而这**不会触发任何
// 权限检查**，因为 qemu 是以一个有权读它的用户在跑。
//
// 因此本项目的做法是**根本不接受用户的绝对路径**：
//
//	用户给的是一个相对于自己存储根的路径，控制面校验后与 user_storage.RootPath
//	拼成绝对路径，只有拼出来的那个绝对路径才会落到本列里。
//
// 把「相对路径」这一约束放在输入处，而不是事后去比对绝对路径的前缀：后者
// 要处理 .. 穿越、符号链接、大小写、尾斜杠等等，而任何一个漏掉都是一个
// 可以直接读宿主机文件系统的漏洞。**少一种表示形式，就少一整类绕过方式。**
//
// 剩下的一种绕过方式控制面拦不住：共享目录里若有一个指向外部的**符号链接**，
// 路径前缀校验看不出来。那必须在节点侧用真实路径解析（realpath）再校验一次
// ——控制面看不到宿主机的文件系统，这一条只能由节点兜底（见 agent.OpShareMount）。
type ShareMount struct {
	ID     int64 `gorm:"primaryKey"`
	VMID   int64 `gorm:"not null;uniqueIndex:uniq_share_mount_vm_tag,priority:1"`
	NodeID int64 `gorm:"not null"`

	// HostPath 是**由控制面拼出来的**绝对路径（RootPath + 用户给的相对路径）。
	// 见类型注释：接口层不接受用户直接给出的绝对路径。
	HostPath string `gorm:"size:512;not null"`

	// Tag 是来宾识别这个挂载点的名字（`mount -t 9p -o trans=virtio <tag>`）。
	//
	// 在**同一台虚拟机内唯一**（uniq_share_mount_vm_tag）：两个共享用同一个
	// tag 时，来宾里只能挂上其中一个，而另一个会以一句「设备忙」或更含糊的
	// 错误失败——用户很难把那个错误与他刚做的操作联系起来。
	Tag string `gorm:"size:64;not null;uniqueIndex:uniq_share_mount_vm_tag,priority:2"`

	SecurityModel string `gorm:"size:16;not null;default:mapped"`

	// ReadOnly **默认为 true**。
	//
	// 可写共享意味着来宾可以往宿主机写文件，而**那些写入不受控制面的配额
	// 约束**——配额是控制面在受理请求时算的，而来宾绕过控制面直接写盘。
	// 一个只读的默认值让「不小心把宿主机写满」不可能发生；确实需要可写时，
	// 用户得显式选一次。
	ReadOnly bool `gorm:"not null;default:true"`

	// MountedAt 是节点上**确认生效**的时刻；为空表示尚未生效。
	//
	// 表结构里没有单独的 status 列，这一列就是状态本身——「有没有挂上」
	// 只有两种真实情况，而失败不属于记录的状态：一条挂载失败的共享**本来
	// 就不存在**，留下记录只会让用户卡在「tag 已存在」上，而他实际什么都
	// 没挂上。失败原因由任务承担（那里才是排查的入口）。
	MountedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (ShareMount) TableName() string { return "share_mount" }

// IsActive 报告该共享是否已在节点上生效。
func (s *ShareMount) IsActive() bool { return s.MountedAt != nil }

// ValidShareSecurityModel 报告安全模型取值是否合法。
func ValidShareSecurityModel(m string) bool {
	switch m {
	case ShareSecurityMapped, ShareSecurityPassthrough, ShareSecurityNone:
		return true
	}
	return false
}
