package agent

// CapabilityArchPrefix 是心跳 capabilities 里的**宿主架构标记**前缀，
// 完整形态为 host.arch.<libvirt 架构名>（如 host.arch.aarch64）。
//
// 为什么复用 capabilities 而不是新增协议字段：架构是「这台节点能跑什么」
// 的宿主事实，与 console.spice 同类；心跳的 capabilities 已是既有的宿主
// 自报通道，加一个点分键即可，不必动 proto 与迁移。
//
// 存在的理由：创建向导的 arch / machine_type / firmware / video_model
// 默认值必须与节点架构一致。旧实现把默认值硬编码成 x86_64 + q35 + bios，
// 在 aarch64 宿主上「什么都不改直接提交」必然被组合校验拒绝（实机测试
// 抓出：每次创建都要手动改正三项）。
const CapabilityArchPrefix = "host.arch."
