package agent

// OpStorageFileDelete 删除节点上「我的存储」里的一份文件（F-5-05）。
//
// 与读取（OpStorageFileRead）同一套寻址：Target 是**相对该用户存储根**的
// 路径，节点侧先找当前根、再回退历史根——存储根切到存储池之前上传的文件
// 仍留在系统盘的旧目录里，只按当前根拼路径会"删不掉"它们。
//
// **文件不存在也算成功**：删除的语义是"让这份内容不在"，而它本来就不在
// 正是想要的结果。控制面因此可以安全重试，也不会因为一次半途而废的历史
// 清理把用户卡在"删不掉"的错误上。
const OpStorageFileDelete OpKind = "storage.file.delete"

// StorageFileDeleteKey 是结果中承载删除详情的键。
const StorageFileDeleteKey = "storage_file_delete"

// StorageFileDeleteInfo 是删除结果。
type StorageFileDeleteInfo struct {
	// RelPath 是请求里的相对路径（回显）。
	RelPath string
	// Deleted 为 false 表示文件本来就不存在（幂等成功）。
	//
	// 与「删掉了」分开返回：控制面据此可以区分「确实清理掉了一份字节」与
	// 「记录指向的东西早就不在了」——后者往往意味着数据早就丢过一次。
	Deleted bool
	// Root 是实际命中的存储根（当前根或历史根）。
	Root string
}
