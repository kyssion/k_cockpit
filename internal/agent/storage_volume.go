package agent

// OpStorageVolumeApply 创建或删除一个 LVM 存储卷（F-5-02）。
//
// 一次操作覆盖创建与删除，由 `action` 区分——两者的参数几乎一样（都只要
// 名称与设备），而拆成两个 OpKind 只会让「哪些参数属于哪一边」这件事需要
// 在两处各维护一遍。
const OpStorageVolumeApply OpKind = "storage.volume.apply"

// VolumeDataKey 是结果中承载补充信息的键。
const VolumeDataKey = "storage_volume"

// VolumeInfo 是节点回传的存储卷信息。
type VolumeInfo struct {
	// Status 是节点侧判断的卷状态（active / sync / degraded / failed）。
	//
	// **由节点给出而不是控制面一律写 active**：镜像卷建好之后还要同步
	// 几小时，期间读正常、写明显变慢。把这段时间标成 active，用户会认为
	// 卷是好的、慢是别的原因——于是一路排查到网络。
	Status string
	// SizeGB 与 PhysicalGB 是节点侧算出的容量。
	//
	// 与控制面的换算**分开返回**：两者不一致说明聚合方式与预期不符
	// （例如镜像份数没有生效），而那种问题如果只信控制面自己算的数字，
	// 就永远发现不了——它会一直显示「占用翻倍」而实际并没有冗余。
	SizeGB     int
	PhysicalGB int
	Message    string
	// Warnings 是节点发现的、值得用户先看一眼的问题。
	Warnings []string
}
