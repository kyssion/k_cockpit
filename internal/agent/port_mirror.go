package agent

// 端口镜像相关操作（F-4-09）。
const (
	// OpPortMirrorEnable 启用镜像，并**同时**在节点侧建立看门狗。
	//
	// 看门狗与镜像必须在**同一次操作**里建立，而且是节点侧的职责。
	// 这不只是实现选择，而是「启用前建立自动回滚看门狗」这句要求的唯一
	// 可行解释：
	//
	//   - 分两次下发会留下一个窗口——镜像已经生效、看门狗还没建起来。
	//     如果网络恰好在那几秒里被镜像打垮，就再也没有东西能撤销它了。
	//   - 把看门狗放在控制面同样不行：控制面是通过网络下发指令的，而
	//     这个功能的失败模式恰恰是**网络断掉**。一个依赖网络的保险，
	//     在它最需要起作用的时候一定不在。
	//
	// 因此 Params 里的 `watchdog_seconds` 是给**节点**的指令：让它自己
	// 计时，到期未收到 `OpPortMirrorConfirm` 就撤销本次变更。节点侧不需要
	// 再向控制面确认任何东西——那样又把网络依赖加回来了。
	OpPortMirrorEnable OpKind = "port_mirror.enable"

	// OpPortMirrorConfirm 取消看门狗（用户确认「保持」）。
	OpPortMirrorConfirm OpKind = "port_mirror.confirm"

	// OpPortMirrorDisable 撤销镜像（用户主动关闭，或看门狗到期后节点自己走的路）。
	OpPortMirrorDisable OpKind = "port_mirror.disable"
)

// PortMirrorDataKey 是结果中承载补充信息的键。
const PortMirrorDataKey = "port_mirror"

// PortMirrorInfo 是节点回传的镜像信息。
type PortMirrorInfo struct {
	// WatchdogSeconds 是节点实际采用的看门狗时长（回显）。
	//
	// 回显而不是让控制面假设：节点可能对时长设了下限或上限，而一个
	// 与控制面记录不一致的看门狗会让「什么时候会被撤销」变得不可预期。
	WatchdogSeconds int
	// Applied 是实际建立的镜像条目数。
	Applied int
	// Message 面向用户的补充说明。
	Message string
	// Warnings 是节点发现的、值得用户先看一眼的问题。
	//
	// 例如「来源接口里有端口当前不在 UP 状态」——那不会让镜像失败，
	// 但会导致抓不到东西，而事后很难判断是配置错了还是本来就没流量。
	Warnings []string
}
