package agent

// OpNetworkCapture 在节点上执行一次限时抓包（F-4-12）。
//
// 它是一个**长时间运行**的操作：下发之后节点要等 duration 秒才返回。
// 这与项目里其它操作不同（那些都在秒级完成），因此有两处要注意：
//
//  1. 任务在跑的时候，界面上应该显示"抓包中，还剩 N 秒"，而不是一个
//     没有进度的转圈。
//  2. 文件路径由**节点**返回而不是控制面指定：文件写在节点自己的磁盘上，
//     它才知道往哪写（以及哪块盘有空间）。控制面拼一个路径出来，很可能
//     写在一个已经满了的分区上。
const OpNetworkCapture OpKind = "network.capture"

// OpNetworkCaptureDelete 删除节点上的抓包文件。
//
// **必须由节点执行**：文件在它的磁盘上，控制面删不掉。而且"文件还在"与
// "文件已删"是两件不同的事——只删控制面记录会让一份含明文流量的文件永远
// 留在宿主机上，而界面上已经看不到它了。
const OpNetworkCaptureDelete OpKind = "network.capture.delete"

// CaptureDataKey 是抓包结果中承载详情的键。
const CaptureDataKey = "network_capture"

// CaptureInfo 是一次抓包的结果。
type CaptureInfo struct {
	// FilePath 是节点上生成的文件路径。
	//
	// 由节点给出而不是控制面指定：文件写在它的磁盘上，它才知道哪里
	// 有空间。
	FilePath string
	// SizeBytes 是文件大小。0 表示抓到了 0 字节——那通常意味着过滤器
	// 没匹配到任何流量，而用户需要知道这一点。
	SizeBytes int64
	Message   string
	// Warnings 是节点侧发现的问题。
	Warnings []string
}
