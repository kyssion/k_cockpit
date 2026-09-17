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

// 重装系统（F-2-11）。
const (
	// OpVMReinstall 用模板重建系统盘，保留硬件配置、数据盘与主网口绑定。
	//
	// 步骤是「先备份、再重建」：把原系统盘改名成备份，然后用模板造一块新的。
	// 顺序反过来（先删旧的再重建）会让失败时的还原变得不可能——而重装失败
	// 往往正是因为磁盘空间不足或模板有问题，那恰恰是最不能丢数据的时刻。
	OpVMReinstall OpKind = "vm.reinstall"

	// OpVMReinstallPurge 清理重装留下的备份盘。
	//
	// 单独成一个操作而不是重装的一部分：备份要在重装**成功之后**继续存在，
	// 什么时候扔掉由用户决定。把它并进重装，等于替用户决定「不再需要回到
	// 原来的系统了」。
	OpVMReinstallPurge OpKind = "vm.reinstall.purge"
)

// ReinstallDataKey 是重装结果中承载磁盘信息的键。
const ReinstallDataKey = "reinstall"

// ReinstallInfo 是节点在重装完成后回传的信息。
type ReinstallInfo struct {
	// BackupPath 是原系统盘被改名到的位置。
	//
	// 由**节点返回**而不是控制面按命名规则推算：真实实现可能因为存储池
	// 不同、快照链不同而把备份放在别处。控制面推算出来的路径一旦对不上，
	// 「还原」与「清理」都会作用在一个不存在的文件上。
	BackupPath string
	// DiskPath 是新系统盘的路径。
	DiskPath string
	// BackingPath 非空表示新系统盘是以某个父盘为底层建的 overlay。
	//
	// 控制面据此把 clone_mode 记成 linked 或 full——**由节点说了算**：
	// 重装要不要走链式是实现的自由（大模板完整复制很慢），而控制面需要
	// 知道的只是「它现在有没有依赖」，那决定了详情页要不要显示依赖提示。
	BackingPath string
}

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
