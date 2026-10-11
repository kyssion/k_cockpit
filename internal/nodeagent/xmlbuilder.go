// xmlbuilder.go：libvirt 域 XML 构建（vm.create/clone 的落点）。
//
// 刻意用 strings.Builder 手拼而不用 encoding/xml：libvirt 域 XML 的形状是
// 固定的，手拼让每个参数与输出的对应关系一目了然（对照 virsh dumpxml 逐行
// 可查），而序列化器会让这层对应关系藏进结构体标签里。
//
// 参数覆盖面（f-2-02 下发的全部创建参数）：核心硬件（CPU/内存/热插拔/拓扑/
// CPU 型号与限额）、机型与固件（aarch64 → virt + UEFI）、磁盘（格式/总线/
// IOPS 限值/多盘）、网卡（型号/桥接或用户态）、显示（VNC + 显卡模型）、
// RTC/APIC/PAE/看门狗/自启。init_mode / hostname / 初始密码属于**来宾内**
// 的自动化（vm.guest，P5），不进域 XML。
package nodeagent

import (
	"fmt"
	"os"
	"strings"
)

// createSpec 是 vm.create/clone 参数的结构化视图。
//
// agent 不直接消费 map：一次显式解码把「参数存在性」翻译清楚（bool 缺省
// 与 false 同义、字符串缺省与 "" 同义），XML 生成只面对确定的值。
type createSpec struct {
	Name     string
	VCPU     int
	MemoryMB int
	DiskGB   int

	DiskFormat string
	DiskBus    string
	OSType     string
	OSVariant  string
	Machine    string
	Firmware   string // bios / uefi
	SecureBoot bool
	BootOrder  string
	AutoStart  bool
	Watchdog   string
	CPUType    string
	CPULimit   int
	APIC       *bool
	PAE        *bool

	NicModel  string
	DataDisks []dataDiskSpec

	IOPSTotal int
	IOPSRead  int
	IOPSWrite int

	VideoModel    string
	RTCMode       string
	Arch          string
	CPUHotplug    bool
	MemoryHotplug bool

	// Clone 相关（克隆走同一构建器，只是磁盘由调用方备好）。
	TemplateDiskPath string
	CloneMode        string

	SystemDiskPath string // 调用方备好的系统盘绝对路径
	ExtraDisks     []extraDisk
}

type dataDiskSpec struct {
	SizeGB  int    `json:"size_gb"`
	Format  string `json:"format"`
	Bus     string `json:"bus"`
	DevHint string `json:"dev"`
}

type extraDisk struct {
	Dev    string
	Path   string
	Bus    string
	Format string
}

// parseCreateSpec 从操作参数解码创建规格。
func parseCreateSpec(target string, params map[string]any) createSpec {
	s := createSpec{Name: target, DiskFormat: "qcow2", DiskBus: "virtio",
		OSType: "linux", Machine: "pc", Firmware: "bios", NicModel: "virtio",
		VideoModel: "virtio", RTCMode: "utc", Arch: "x86_64"}
	if v, ok := params["vcpu"].(float64); ok {
		s.VCPU = int(v)
	}
	if v, ok := params["memory_mb"].(float64); ok {
		s.MemoryMB = int(v)
	}
	if v, ok := params["disk_gb"].(float64); ok {
		s.DiskGB = int(v)
	}
	str := func(key, def string) string {
		if v, ok := params[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
		return def
	}
	s.DiskFormat = str("disk_format", s.DiskFormat)
	s.DiskBus = str("disk_bus", s.DiskBus)
	s.OSType = str("os_type", s.OSType)
	s.OSVariant = str("os_variant", "")
	s.Machine = str("machine_type", s.Machine)
	s.Firmware = str("firmware", s.Firmware)
	s.BootOrder = str("boot_order", "")
	s.Watchdog = str("watchdog", "")
	s.CPUType = str("cpu_type", "")
	s.NicModel = str("nic_model", s.NicModel)
	s.VideoModel = str("video_model", s.VideoModel)
	s.RTCMode = str("rtc_mode", s.RTCMode)
	s.Arch = str("arch", s.Arch)
	s.CloneMode = str("clone_mode", "")
	s.TemplateDiskPath = str("template_disk_path", "")
	b := func(key string) bool { v, _ := params[key].(bool); return v }
	s.SecureBoot = b("secure_boot")
	s.AutoStart = b("auto_start")
	s.CPUHotplug = b("cpu_hotplug")
	s.MemoryHotplug = b("memory_hotplug")
	if v, ok := params["cpu_limit_percent"].(float64); ok {
		s.CPULimit = int(v)
	}
	if v, ok := params["apic"].(bool); ok {
		s.APIC = &v
	}
	if v, ok := params["pae"].(bool); ok {
		s.PAE = &v
	}
	if v, ok := params["disk_iops_total"].(float64); ok {
		s.IOPSTotal = int(v)
	}
	if v, ok := params["disk_iops_read"].(float64); ok {
		s.IOPSRead = int(v)
	}
	if v, ok := params["disk_iops_write"].(float64); ok {
		s.IOPSWrite = int(v)
	}
	if v, ok := params["data_disks"]; ok && v != nil {
		if raw, err := jsonMarshal(v); err == nil {
			_ = jsonUnmarshalTo(raw, &s.DataDisks)
		}
	}
	return s
}

// BuildDomainXML 生成域定义。
//
// 生成顺序对照 virsh dumpxml 的习惯排版，逐段可对照核对。网络默认接
// **用户态栈（user）**：它不依赖任何桥存在、天然出网（SLIRP NAT），
// P3 的交换机/桥接接入后由 vm.interface.change 替换为真实桥接口——
// 创建时机器还没有任何已定义的交换机可接，这是唯一不会失败的默认值。
func (s createSpec) BuildDomainXML() string {
	var b strings.Builder
	b.WriteString("<domain type='kvm'>\n")
	fmt.Fprintf(&b, "  <name>%s</name>\n", xmlEsc(s.Name))

	// 内存与 vCPU（热添加需要 maximum 声明）。
	if s.MemoryHotplug {
		fmt.Fprintf(&b, "  <maximumMemory unit='KiB'>%d</maximumMemory>\n", s.MemoryMB*1024*8)
	}
	fmt.Fprintf(&b, "  <memory unit='KiB'>%d</memory>\n", s.MemoryMB*1024)
	if s.CPUHotplug {
		fmt.Fprintf(&b, "  <vcpu current='%d' placement='static'>%d</vcpu>\n", s.VCPU, s.VCPU*4)
	} else {
		fmt.Fprintf(&b, "  <vcpu placement='static'>%d</vcpu>\n", s.VCPU)
	}

	// OS / 机型 / 固件。
	io := "hvm"
	arch := s.Arch
	if arch == "" {
		arch = "x86_64"
	}
	machine := s.Machine
	fmt.Fprintf(&b, "  <os>\n    <type arch='%s' machine='%s'>%s</type>\n", xmlEsc(arch), xmlEsc(machine), io)
	if s.Firmware == "uefi" {
		// UEFI 固件：优先显式 loader 路径（发行版差异集中在 candidate 探测，
		// 全部缺席时退回 firmware efi='yes' 让 libvirt 自己找描述符）。
		// 显式路径是实测最可靠的形态：firmware 自动选择依赖
		// /usr/share/qemu/firmware/*.json 描述符，精简安装常缺。
		if loader, vars := uefiCandidates(arch); loader != "" {
			fmt.Fprintf(&b, "    <loader readonly='yes' type='pflash'>%s</loader>\n", xmlEsc(loader))
			fmt.Fprintf(&b, "    <nvram template='%s'>/var/lib/libvirt/qemu/nvram/%s_VARS.fd</nvram>\n",
				xmlEsc(vars), xmlEsc(s.Name))
		} else {
			b.WriteString("    <firmware efi='yes'/>\n")
		}
	}
	if bo := normalizeBootOrder(s.BootOrder); bo != "" {
		for _, dev := range strings.Split(bo, ",") {
			fmt.Fprintf(&b, "    <boot dev='%s'/>\n", xmlEsc(dev))
		}
	}
	b.WriteString("  </os>\n")

	// CPU：host-passthrough / host-model 是**模式**不是型号——它们不能进
	// <model>（custom+model 组合是无效 XML，define 会直接拒绝）。只有具名
	// 型号（qemu64 等）才走 custom+model。
	cputune := ""
	if s.CPULimit > 0 && s.CPULimit < 100 {
		cputune = fmt.Sprintf("<cputune><quota>%d</quota></cputune>", s.CPULimit*s.VCPU*10000/100)
	}
	switch {
	case s.CPUType == "" || s.CPUType == "host-passthrough":
		fmt.Fprintf(&b, "  <cpu mode='host-passthrough'/>\n%s\n", cputune)
	case s.CPUType == "host-model":
		fmt.Fprintf(&b, "  <cpu mode='host-model'/>\n%s\n", cputune)
	default:
		fmt.Fprintf(&b, "  <cpu mode='custom' match='exact' check='none'>\n"+
			"    <model fallback='forbid'>%s</model>\n  </cpu>\n%s\n", xmlEsc(s.CPUType), cputune)
	}

	// 特性开关：aarch64/virt 上 acpi 必开（否则固件起不来），APIC/PAE 是
	// x86 概念。
	b.WriteString("  <features>\n    <acpi/>\n")
	if arch != "aarch64" {
		if s.APIC == nil || *s.APIC {
			b.WriteString("    <apic/>\n")
		}
		if s.PAE != nil && *s.PAE {
			b.WriteString("    <pae/>\n")
		}
	}
	b.WriteString("  </features>\n")

	fmt.Fprintf(&b, "  <clock offset='%s'/>\n", xmlEsc(s.RTCMode))
	b.WriteString("  <on_poweroff>destroy</on_poweroff>\n  <on_reboot>restart</on_reboot>\n  <on_crash>destroy</on_crash>\n")

	// 设备。
	b.WriteString("  <devices>\n")
	// 系统盘 + 数据盘。
	if s.SystemDiskPath != "" {
		b.WriteString(s.diskXML("vda", s.SystemDiskPath, s.DiskBus, s.DiskFormat, s.IOPSTotal, s.IOPSRead, s.IOPSWrite, false))
	}
	for i, d := range s.DataDisks {
		dev := d.DevHint
		if dev == "" {
			dev = nextDataDev(i)
		}
		bus := d.Bus
		if bus == "" {
			bus = s.DiskBus
		}
		format := d.Format
		if format == "" {
			format = s.DiskFormat
		}
		path := dataDiskPath(s.Name, dev, format)
		b.WriteString(s.diskXML(dev, path, bus, format, 0, 0, 0, false))
	}
	// scsi 总线需要控制器。
	if strings.Contains(b.String(), "bus='scsi'") {
		b.WriteString("    <controller type='scsi' index='0' model='virtio-scsi'/>\n")
	}
	// aarch64/virt 机型没有隐式 USB 控制器，而 USB tablet 输入需要它——
	// 缺了它 define 直接拒绝（"USB is disabled"）。
	if arch == "aarch64" {
		b.WriteString("    <controller type='usb' model='qemu-xhci'/>\n")
	}
	// 网卡。
	fmt.Fprintf(&b, "    <interface type='user'>\n      <model type='%s'/>\n    </interface>\n", xmlEsc(s.NicModel))
	// 显示：VNC（无密码，监听本机——对外暴露是独立的高危操作）+ 显卡。
	b.WriteString("    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1'/>\n")
	fmt.Fprintf(&b, "    <video><model type='%s'/></video>\n", xmlEsc(videoOrDefault(s.VideoModel, arch)))
	// 输入与控制台：tablet 让 VNC 里的指针定位正确。ps2 输入是 x86 概念
	// （aarch64 virt 上 define 直接拒绝 "ps2 is not supported"）。
	b.WriteString("    <input type='tablet' bus='usb'/>\n")
	if arch != "aarch64" {
		b.WriteString("    <input type='mouse' bus='ps2'/>\n")
	}
	b.WriteString("    <console type='pty'/>\n")
	b.WriteString("    <memballoon model='virtio'/>\n")
	if w := s.Watchdog; w != "" && w != "none" {
		fmt.Fprintf(&b, "    <watchdog model='i6300esb' action='%s'/>\n", xmlEsc(w))
	}
	b.WriteString("  </devices>\n</domain>\n")
	return b.String()
}

// diskXML 渲染一块磁盘。
func (s createSpec) diskXML(dev, path, bus, format string, iopsTotal, iopsRead, iopsWrite int, readonly bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "    <disk type='file' device='disk'>\n      <driver name='qemu' type='%s' cache='writeback'/>\n", xmlEsc(format))
	fmt.Fprintf(&b, "      <source file='%s'/>\n", xmlEsc(path))
	fmt.Fprintf(&b, "      <target dev='%s' bus='%s'/>\n", xmlEsc(dev), xmlEsc(bus))
	if iopsTotal > 0 {
		fmt.Fprintf(&b, "      <iotune><total_iops_sec>%d</total_iops_sec></iotune>\n", iopsTotal)
	} else if iopsRead > 0 || iopsWrite > 0 {
		b.WriteString("      <iotune>")
		if iopsRead > 0 {
			fmt.Fprintf(&b, "<read_iops_sec>%d</read_iops_sec>", iopsRead)
		}
		if iopsWrite > 0 {
			fmt.Fprintf(&b, "<write_iops_sec>%d</write_iops_sec>", iopsWrite)
		}
		b.WriteString("</iotune>\n")
	}
	if readonly {
		b.WriteString("      <readonly/>\n")
	}
	b.WriteString("    </disk>\n")
	return b.String()
}

// normalizeBootOrder 把 boot_order 归一为 libvirt 的设备枚举（缺省磁盘优先）。
func normalizeBootOrder(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "hd"
	}
	return s
}

// uefiCandidates 返回 (loader, vars) 的 UEFI 固件路径候选。
// 顺序即优先级：Ubuntu/Debian 的 AAVMF（aarch64）与 OVMF（x86_64）。
// 都不存在返回空——调用方退回 firmware efi='yes'。
func uefiCandidates(arch string) (loader, vars string) {
	candidates := map[string][2]string{
		"aarch64": {"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
		"x86_64":  {"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
	}
	if c, ok := candidates[arch]; ok {
		if _, err := os.Stat(c[0]); err == nil {
			return c[0], c[1]
		}
	}
	return "", ""
}

// videoOrDefault ramfb 是 aarch64 固件帧缓冲（virtio 显卡在固件阶段不可用）。
func videoOrDefault(model, arch string) string {
	if model != "" && model != "none" {
		return model
	}
	if arch == "aarch64" {
		return "ramfb"
	}
	return "vga"
}

// nextDataDev 第 i 块数据盘的目标设备名（vdb 起）。
func nextDataDev(i int) string {
	return "vd" + string(rune('b'+i))
}

// dataDiskPath 数据盘的默认落盘路径。
func dataDiskPath(vm, dev, format string) string {
	ext := format
	if ext == "" {
		ext = "qcow2"
	}
	return fmt.Sprintf("/var/lib/k_cockpit/images/%s-%s.%s", vm, dev, ext)
}

// xmlEsc 转义 XML 属性/文本的五个保留字符。
func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", "\"", "&quot;")
	return r.Replace(s)
}
