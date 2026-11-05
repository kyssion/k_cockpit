package agent

// CapabilitySPICE 是心跳 capabilities 里的**宿主能力标记**，不是 OpKind。
//
// 节点的 OpKind 注册表回答「这台节点能执行哪些操作」，而 SPICE 是否可用
// 取决于 libvirt 的编译期能力，属于宿主探测结果。控制面收到该标记后同步
// 该节点上虚拟机的 spice_supported；标记消失时同样会置回不支持。
const CapabilitySPICE = "console.spice"
