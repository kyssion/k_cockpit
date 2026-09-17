package model

import "time"

// UserStorage 对应 user_storage 表：用户在某个节点上的存储配额（F-9-02）。
//
// 配额是**按用户按节点**给的，而不是全局面板一个数字：磁盘就在那台宿主机上，
// 一个用户在 A 节点的用量与 B 节点无关——合成一个总数会让「A 满了」影响到
// B 上的操作，而两者根本没有共用任何资源。
type UserStorage struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int64 `gorm:"not null;uniqueIndex:uniq_user_storage_user_node,priority:1"`
	NodeID int64 `gorm:"not null;uniqueIndex:uniq_user_storage_user_node,priority:2"`

	// Enabled 表示该用户在这个节点上启用了独立存储空间。
	//
	// 关闭时**不做配额限制**——与 quota_bytes = 0 是同一件事的两种写法，
	// 保留这个字段是因为它还有界面含义（「这个用户有没有开独立空间」），
	// 而那与「开了但没限额」是两回事。
	Enabled bool `gorm:"not null;default:false"`

	// QuotaBytes 是配额上限；**0 表示不限制**。
	//
	// 0 作为「不限」而不是「零额度」：新建的记录默认就是 0，若解释成零额度，
	// 那么任何一次「先建记录再设配额」的操作都会在中间那一刻把所有写入挡死。
	QuotaBytes int64 `gorm:"not null;default:0"`

	// UsedBytes 是**用量缓存**，不是权威来源。
	//
	// 权威是各资源表（vm / vm_export / template）里的实际数据。这个列存在的
	// 意义是让「列出所有用户的用量」这种查询不必对每行都做一次 SUM——它由
	// quota 服务在每次算出用量时刷新（见 quota.Service.Usage）。
	//
	// **不要把它当作判断依据**：它可能比真实值旧。判断超限一律现算。
	UsedBytes int64 `gorm:"not null;default:0"`

	// ReadOnly 为 true 时该用户在此节点上只能读，不能新建任何占空间的资源。
	ReadOnly bool `gorm:"not null;default:false"`

	// RootPath 是该用户空间的根目录（由节点创建并回报）。
	RootPath      *string `gorm:"size:512"`
	InitializedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (UserStorage) TableName() string { return "user_storage" }

// IsUnlimited 报告该配额是否不设上限。
func (u *UserStorage) IsUnlimited() bool {
	return u == nil || !u.Enabled || u.QuotaBytes <= 0
}

// Remaining 返回剩余额度；不限额时返回 -1。
//
// 用 -1 而不是 math.MaxInt64：界面需要区分「还有很多」与「不限」——
// 前者会随用量变化，后者永远不会。一个显示成 8 EiB 的剩余空间既不可信
// 也不好看。
func (u *UserStorage) Remaining(used int64) int64 {
	if u.IsUnlimited() {
		return -1
	}
	remain := u.QuotaBytes - used
	if remain < 0 {
		return 0
	}
	return remain
}
