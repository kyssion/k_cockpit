package agent

// 模板相关操作（F-3-01 / F-3-02）。
//
// 「模板」在协议层就是**一块可写的系统盘副本**加一组默认硬件参数。没有更复杂
// 的东西：克隆要么复制它（full），要么以它为 backing file 建 overlay（linked）。
const (
	// OpTemplatePrepare 从一台虚拟机的系统盘制备模板。
	//
	// **要求源虚拟机关机**：运行中的系统盘在被复制的同时还在被写入，复制出来
	// 的模板会是一个**崩溃一致性**的快照——文件系统日志可能处于未提交状态，
	// 拿它创建的虚拟机开机时要做 fsck，运气不好就是只读挂载。这不是理论问题，
	// 而是「为什么我的克隆机启动后是只读」的最常见原因。
	OpTemplatePrepare OpKind = "template.prepare"

	// OpTemplateDelete 删除模板盘。
	OpTemplateDelete OpKind = "template.delete"

	// OpVMClone 从模板克隆出一台虚拟机。
	OpVMClone OpKind = "vm.clone"
)

// TemplateDataKey 是模板制备结果中承载磁盘信息的键。
const TemplateDataKey = "template"

// TemplateInfo 是节点在制备模板后回传的信息。
type TemplateInfo struct {
	// DiskPath 是模板盘在宿主机上的路径。
	DiskPath string
	// SizeGB 是模板盘的实际大小。
	SizeGB int
	// Format 是磁盘格式（qcow2 / raw）。
	Format string
}

// CloneDataKey 是克隆结果中承载磁盘信息的键。
const CloneDataKey = "clone"

// CloneInfo 是节点在克隆完成后回传的信息。
type CloneInfo struct {
	// DiskPath 是新虚拟机的系统盘路径。
	DiskPath string
	// BackingPath 是链式克隆的父盘路径；完整克隆为空。
	//
	// 由**节点返回**而不是控制面按模板路径推算：真实实现可能会为了性能
	// 把模板盘放到别处（比如 SSD 缓存层），由节点说了算才不会对不上。
	BackingPath string
}
