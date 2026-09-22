package agent

// OpCaptureFetch 按节点上的文件路径取回一份抓包文件（G-38）。
//
// 与导出产物取回（OpVMExportFetch）是同一个模式：控制面转发字节而不给
// 浏览器一个节点直链——直链意味着暴露节点访问凭据或开一个匿名入口，
// 而抓包文件里是完整的流量内容，比导出产物更敏感。
const OpCaptureFetch OpKind = "network.capture.fetch"

// CaptureContentKey 是 Execute 结果里文件内容的键。
const CaptureContentKey = "capture_content"
