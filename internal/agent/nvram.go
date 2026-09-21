package agent

// NVRAMDataKey 是 NVRAM 修复结果中承载数据的键。
const NVRAMDataKey = "nvram"

// NVRAMRepairInfo 是 UEFI 启动项修复的结果。
//
// 带上重建出来的启动项名与路径，而不只是一句"已修复"：用户接下来要做的事
// 是**重启并确认**，而他要确认的正是"现在从哪启动"。没有这两项，他只能靠
// 开机试一次来验证——而如果这次仍然失败，他就无从判断是没修好还是修歪了。
type NVRAMRepairInfo struct {
	// BootEntry 是固件里的启动项编号（如 Boot0001）。
	BootEntry string
	// BootPath 是启动项指向的 EFI 文件路径。
	BootPath string
	// RebootNeeded 为 true 表示需要重启虚拟机才会生效。
	RebootNeeded bool
	Message      string
}
