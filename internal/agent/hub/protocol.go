package hub

import "regexp"

// agentProtocolVersion 是控制面当前实现的协议版本，与
// internal/nodeagent.ProtocolVersion 同值。刻意不跨包引用：hub 是控制面
// 传输层，依赖节点侧包会颠倒依赖方向。
const agentProtocolVersion = 1

// minSupportedProtocolVersion 是仍接受的最低版本（proto 的 N-1 兼容承诺）。
//
// 当前 N=1，N-1=0 表示「未声明版本」而不是一个真实协议版本；接受它等于
// 让旧 agent 静默接入一套它不理解的契约，因此下限同样取 1。
const minSupportedProtocolVersion = 1

// protocolVersionSupported 判断控制面能否与该 agent 通话。
func protocolVersionSupported(v int32) bool {
	return v >= minSupportedProtocolVersion && v <= agentProtocolVersion
}

// nodeNamePattern 与 service/node 的节点名规则一致。传输层必须自己校验：
// 名字来自节点上报，不能假设对端会遵守控制面的业务规则。
var nodeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func validNodeName(s string) bool { return nodeNamePattern.MatchString(s) }
