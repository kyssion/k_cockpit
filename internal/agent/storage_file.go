package agent

// OpStorageRootSet 把「我的存储」的节点侧根目录切换到指定路径。
//
// 参数 `path` 为绝对路径（默认存储池挂载点下的 `userdata`）；为空表示
// 没有可用存储池，回到历史默认根。节点侧只接受落在自己管理的池挂载点
// 之下的路径——控制面被攻破也不能把用户存储指到 /etc。
//
// 它存在的理由：用户存储此前写死在系统盘上，节点建了数据池也用不上，
// 大镜像会把根分区写满（实机抓出）。根目录属于**节点级配置**，与池的
// 默认标记同源，因此由控制面收敛而不是让用户在节点上手工改。
const OpStorageRootSet OpKind = "storage.root.set"

// OpStorageFileCommit 把控制面暂存区里的完整内容落到节点的用户存储空间。
//
// 分片上传的字节先收在控制面本地（见 userstorage.ChunkStore 的说明），
// 全部到齐并拼接完成后，由这个操作把整份内容交给节点。
//
// **它是整份提交，而不是逐片转发**：逐片转发意味着网络中断会留下一堆
// 半截分片散落在节点上，而清理它们需要一个额外的协调过程；整份提交则
// 要么成功要么没有，节点侧不需要维护「哪些片收到了」这个状态——那个状态
// 控制面已经有了（bitmap），没必要有两份。
const OpStorageFileCommit OpKind = "storage.file.commit"

// StorageFileDataKey 是结果中承载补充信息的键。
const StorageFileDataKey = "storage_file"

// StorageFileInfo 是节点回传的信息。
type StorageFileInfo struct {
	// RelPath 是文件最终落在用户存储空间里的相对路径（回显）。
	RelPath string
	// SizeBytes 是节点实际写入的字节数。
	//
	// 与请求里的期望大小**分开返回**：两者不一致说明传输过程中出了问题，
	// 而那种问题如果只信控制面自己算的数字，就永远发现不了。
	SizeBytes int64
	// Checksum 是节点侧算出的摘要。
	//
	// 契约要求它与控制面算出的**必须一致**——不一致时调用方应当把文件
	// 判为失败并清理，而不是登记一条指向损坏内容的记录。
	Checksum string
	Message  string
}
