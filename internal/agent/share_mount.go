package agent

// 目录共享相关操作（F-5-06）。
const (
	// OpShareMount 把一个宿主机目录以 9p VirtFS 挂进虚拟机（或卸载）。
	//
	// 两个动作共用一个操作，由 Params["action"]（mount / unmount）区分：
	// 对节点而言是同一件事的两个取值——让这个 tag 出现在来宾里 /
	// 让它不再出现。
	//
	// **节点侧必须做一次真实路径校验**。控制面只能保证「用户给的是相对路径、
	// 拼出来的绝对路径在自己存储根之下」，但它看不到宿主机的文件系统：共享
	// 目录里若有一个指向外部的符号链接，前缀校验完全看不出来，而来宾会顺着
	// 它读到宿主机上的任意文件。真实路径解析（realpath / EvalSymlinks）只有
	// 节点做得到，因此这一条**必须由节点兜底**，且失败必须返回失败——
	// 静默放行等于把控制面那道校验一起作废。
	OpShareMount OpKind = "vm.share.mount"
)

// ShareDataKey 是结果中承载补充信息的键。
const ShareDataKey = "share"

// ShareInfo 是节点回传的共享信息。
type ShareInfo struct {
	// Tag 是最终生效的 tag，回显给调用方。
	Tag string
	// Message 面向用户的补充说明。
	Message string
	// Warnings 是节点发现、值得用户看一眼的问题。
	Warnings []string
}
