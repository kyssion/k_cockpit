package agent

// OpVMDisksIndependent 把链接克隆的磁盘变成独立盘（解除对父盘的依赖）。
//
// 链接克隆的磁盘**只是模板之上的一层覆盖**——它记录的是"相对模板改了什么"。
// 这让克隆很快、很省空间，代价是**父盘删不掉**：父盘一没，那些虚拟机就
// 全废了。解除依赖做的事就是把那层覆盖合并成一个完整的镜像。
//
// 两条约束：
//
//  1. **必须停机**。它要复制整个镜像（可能几十 GB），而运行中的机器还在往
//     那层覆盖里写——复制出来的东西与"某一瞬间"对不上。
//  2. **失败必须保持原状**。合并中途失败时父盘依赖仍然存在，因此控制面那边
//     的标记**不能先改**。标记先改成 full 的话，控制面会说"这台机器已经独立"
//     而实际它还依赖着父盘——那时用户去删模板，删掉了，然后一堆机器坏掉。
const OpVMDisksIndependent OpKind = "vm.disks.independent"

// VMDiskIndependentKey 是结果中承载补充信息的键。
const VMDiskIndependentKey = "vm_disks_independent"

// VMDiskIndependentInfo 是解除依赖的结果。
type VMDiskIndependentInfo struct {
	// Applied 为 true 表示磁盘已经是独立盘。
	Applied bool
	// FreedFrom 是被解除依赖的父盘描述（模板名或父虚拟机名）。
	FreedFrom string
	// SizeBytes 是合并后的镜像大小，供界面展示"占了多少空间"。
	SizeBytes int64
	Message   string
}
