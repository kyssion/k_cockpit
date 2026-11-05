package agent

// vm.config.update 的键（与 service/compute/vm 的编辑矩阵同名）。
//
// 这里集中定义是为了让**控制面与节点对"哪些键能生效"有一致的答案**：
// 控制面在受理时据此拒绝，节点在执行时据此兜底。新增一项时同时改
// 本文件、nodeagent 的 applyConfigXML / vmConfigUpdate 与编辑矩阵。
const (
	ConfigKeyVCPU            = "vcpu"
	ConfigKeyMemoryMB        = "memory_mb"
	ConfigKeyAutoStart       = "auto_start"
	ConfigKeyDiskIOPSTotal   = "disk_iops_total"
	ConfigKeyDiskIOPSRead    = "disk_iops_read"
	ConfigKeyDiskIOPSWrite   = "disk_iops_write"
	ConfigKeyDiskBytesTotal  = "disk_bytes_total"
	ConfigKeyDiskBytesRead   = "disk_bytes_read"
	ConfigKeyDiskBytesWrite  = "disk_bytes_write"
	ConfigKeyBootOrder       = "boot_order"
	ConfigKeyVideoModel      = "video_model"
	ConfigKeyRTCMode         = "rtc_mode"
	ConfigKeyWatchdog        = "watchdog"
	ConfigKeyAPIC            = "apic"
	ConfigKeyPAE             = "pae"
	ConfigKeyHideKVM         = "hide_kvm"
	ConfigKeyMemoryHugepages = "memory_hugepages"
	ConfigKeyCPUSockets      = "cpu_sockets"
	ConfigKeyCPUCores        = "cpu_cores"
	ConfigKeyCPUThreads      = "cpu_threads"
	ConfigKeyCPUAffinity     = "cpu_affinity"
	ConfigKeyCPULimitPercent = "cpu_limit_percent"
)

// AppliedConfigKeys 是**节点侧当前真实落实**的配置键。
//
// 不在这个集合里的键（机型、固件、架构、热插拔槽位、嵌套虚拟化等）要么
// 需要重建域、要么尚无节点实现。控制面必须在受理阶段拒绝它们——放行会
// 得到"任务成功、虚拟机没变"，那比直接报错更难排查。
var AppliedConfigKeys = map[string]struct{}{
	ConfigKeyVCPU:            {},
	ConfigKeyMemoryMB:        {},
	ConfigKeyAutoStart:       {},
	ConfigKeyDiskIOPSTotal:   {},
	ConfigKeyDiskIOPSRead:    {},
	ConfigKeyDiskIOPSWrite:   {},
	ConfigKeyDiskBytesTotal:  {},
	ConfigKeyDiskBytesRead:   {},
	ConfigKeyDiskBytesWrite:  {},
	ConfigKeyBootOrder:       {},
	ConfigKeyVideoModel:      {},
	ConfigKeyRTCMode:         {},
	ConfigKeyWatchdog:        {},
	ConfigKeyAPIC:            {},
	ConfigKeyPAE:             {},
	ConfigKeyHideKVM:         {},
	ConfigKeyMemoryHugepages: {},
	ConfigKeyCPUSockets:      {},
	ConfigKeyCPUCores:        {},
	ConfigKeyCPUThreads:      {},
	ConfigKeyCPUAffinity:     {},
	ConfigKeyCPULimitPercent: {},
}

// ConfigKeyApplied 报告某个配置键是否由节点侧真正落实。
func ConfigKeyApplied(key string) bool {
	_, ok := AppliedConfigKeys[key]
	return ok
}
