package agent

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
