package agent

// OpStorageFileRead 按节点上的相对路径取回一份用户存储文件（G-38）。
//
// 与抓包取回、导出取回同一模式：控制面转发字节，归属校验留在控制面。
// Target 是文件的相对路径（相对该用户的存储根），节点侧据此定位。
const OpStorageFileRead OpKind = "storage.file.read"

// StorageFileContentKey 是 Execute 结果里文件内容的键。
const StorageFileContentKey = "storage_file_content"
