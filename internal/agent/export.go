package agent

// 导出（F-2-14）。
const (
	// OpVMExport 把虚拟机的系统盘（可选数据盘）导出为镜像或 OVA 包。
	//
	// **要求关机的理由比其它操作更强**：导出的产物会被**搬到别的地方使用**
	// （导入到另一套环境、当作模板、交给别人），因此它必须是一个干净的、
	// 自洽的镜像。运行中导出得到的是崩溃一致性快照——文件系统日志可能未提交，
	// 导入方开机时要做 fsck，运气不好就是只读挂载。而导入方往往不在你手边，
	// 出了问题很难回头找原因。
	OpVMExport OpKind = "vm.export"

	// OpVMExportDelete 删除导出产物。
	OpVMExportDelete OpKind = "vm.export.delete"

	// OpVMExportFetch 读取导出产物的内容，供控制面转发给用户下载。
	//
	// 让控制面**转发**而不是给用户一个节点上的直链：直链意味着要把节点的
	// 访问凭据或一个匿名可访问的地址暴露出去，而产物里是整台机器的数据。
	// 转发多花一次带宽，但权限判断留在控制面这一处。
	OpVMExportFetch OpKind = "vm.export.fetch"
)

// ExportDataKey 是 OpVMExport 结果中承载产物信息的键。
const ExportDataKey = "export"

// ExportInfo 是节点在导出完成后回传的信息。
type ExportInfo struct {
	// FilePath 是产物在宿主机上的路径。
	FilePath string
	// FileName 是提供给用户的下载文件名（含扩展名）。
	FileName string
	// SizeBytes 是产物大小，**计入用户的存储配额**（f-2-14）。
	SizeBytes int64
}

// ExportContentKey 是 OpVMExportFetch 结果中承载产物字节的键。
const ExportContentKey = "content"

// ExportContent 是导出产物的内容。
type ExportContent struct {
	// Data 是**原始字节**，由调用方决定如何编码。
	//
	// 不在协议层转 base64：那会让每一层都以为「产物本来就是字符串」，
	// 而它实际是二进制——将来换成流式传输时，这个假设会变成障碍。
	Data []byte
	// MIME 是产物的媒体类型。
	MIME string
}
