// xmlbuilder.go：libvirt 域 XML 构建（vm.create/clone 的落点）。
//
// 刻意用 strings.Builder 手拼而不用 encoding/xml：libvirt 域 XML 的形状是
// 固定的，手拼让每个参数与输出的对应关系一目了然（对照 virsh dumpxml 逐行
// 可查），而序列化器会让这层对应关系藏进结构体标签里。
//
// 参数覆盖面（f-2-02 下发的全部创建参数）：核心硬件（CPU/内存/热插拔/拓扑/
// CPU 型号与限额）、机型与固件（aarch64 → virt + UEFI）、磁盘（格式/总线/
// IOPS 限值/多盘）、网卡（型号/桥接或用户态）、显示（VNC + 显卡模型）、
// RTC/APIC/PAE/看门狗/自启。init_mode / hostname / 初始密码不进域 XML：
// 它们由独立生成的 cloud-init / ConfigDrive 介质承载（seed.go，F-2-17），
// 建域完成后挂成第二块光驱。
package nodeagent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

	NicModel string
	// NicBridge 是主网口要接入的宿主机网桥。空表示用户态网络（SLIRP）。
	//
	// 空值必须保留一条可用路径：节点上还没有任何网桥时（刚纳管、还没建
	// 网络），创建虚拟机仍然要能成功——但那条路径**没有**端口转发、安全组
	// 与 DHCP，因此调用方会把"退化为用户态"如实告诉用户，而不是假装接上了。
	NicBridge string
	// NicBridgeOVS 表示 NicBridge 是 OVS 网桥。
	//
	// 必须与普通 Linux 桥区分：OVS 桥上如果不带 `<virtualport type='openvswitch'/>`，
	// libvirt 会走 Linux 桥的 ioctl 路径，开机直接失败
	// （"Unable to add bridge kbrX port vnet0: Operation not supported"，实机抓出）。
	NicBridgeOVS bool
	DataDisks    []dataDiskSpec

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
	// 架构/机型/固件的默认值取自**节点自身**：agent 与宿主机同架构编译
	// 部署（发布制品按 amd64/arm64 分包），runtime.GOARCH 即宿主架构。
	// 旧实现硬编码 x86_64 + pc，aarch64 节点上建虚拟机必然失败：
	// "unsupported configuration: No emulator found for arch 'x86_64'"
	// （实机测试抓出）。aarch64 没有 pc 机型（对应 virt），常规引导也
	// 依赖 UEFI（AAVMF），因此固件默认值同样随架构走——显式传入的参数
	// 仍然优先。
	defArch := libvirtArch(runtime.GOARCH)
	s := createSpec{Name: target, DiskFormat: "qcow2", DiskBus: "virtio",
		OSType: "linux", Machine: defaultMachine(defArch),
		Firmware: defaultFirmware(defArch), NicModel: "virtio",
		VideoModel: "virtio", RTCMode: "utc", Arch: defArch}
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
	s.NicBridge = str("bridge_name", "")
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
	// 网卡：选了交换机就接网桥，否则退回用户态网络。
	if s.NicBridge != "" {
		fmt.Fprintf(&b, "    <interface type='bridge'>\n      <source bridge='%s'/>\n", xmlEsc(s.NicBridge))
		if s.NicBridgeOVS {
			b.WriteString("      <virtualport type='openvswitch'/>\n")
		}
		fmt.Fprintf(&b, "      <model type='%s'/>\n    </interface>\n", xmlEsc(s.NicModel))
	} else {
		fmt.Fprintf(&b, "    <interface type='user'>\n      <model type='%s'/>\n    </interface>\n", xmlEsc(s.NicModel))
	}
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
	// QEMU guest agent 通道（virtio-serial）。来宾内改密、读 IP、就绪探测、
	// 自动弹介质都走它；没有它 `virsh qemu-agent-command` 直接报
	// "QEMU guest agent is not configured"（第 3 轮 DEF-R3-16）。
	// 控制器与通道的顺序同 virsh dumpxml：控制器在前。
	if !hasVirtioSerialController(b.String()) {
		b.WriteString("    <controller type='virtio-serial' index='0'/>\n")
	}
	b.WriteString(guestAgentChannelXML("    "))
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
	// 界面口径是 disk,cdrom,network，而域 XML 只认 hd/cdrom/network/fd。
	// 不做这层翻译，创建出的定义会被 libvirt 拒绝（"unknown boot device"）。
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if d := bootDevName(p); d != "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ",")
}

// libvirtArch 把 Go 的架构名映射为 libvirt/QEMU 的架构名：arm64 → aarch64
// （Linux uname 亦用 aarch64）；amd64 与 x86_64 的差异只剩名字，其余架构
// 不在本产品支持范围（PRD Q-6：x86_64 + aarch64 先行），原样透传即可。
func libvirtArch(goarch string) string {
	if goarch == "arm64" {
		return "aarch64"
	}
	return goarch
}

// defaultMachine 返回架构的默认机型：x86_64 用 pc，aarch64 只有 virt。
func defaultMachine(arch string) string {
	if arch == "aarch64" {
		return "virt"
	}
	return "pc"
}

// defaultFirmware 返回架构的默认固件：aarch64 的 virt 机型没有 BIOS，
// 常规引导依赖 UEFI（AAVMF，发行版随 qemu-efi-aarch64 提供）；x86_64
// 维持 bios（SeaBIOS 内建，无额外依赖）。
func defaultFirmware(arch string) string {
	if arch == "aarch64" {
		return "uefi"
	}
	return "bios"
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
//
// 根目录用 imagesDir 而不是硬编码 `/var/lib/k_cockpit/images`：域 XML 里
// 写的路径与创建时真正落盘的路径（vmCreate 用 imagesDir）必须是同一个根，
// 否则测试把根指到临时目录后，XML 仍指向 /var/lib——两边漂移。
func dataDiskPath(vm, dev, format string) string {
	ext := format
	if ext == "" {
		ext = "qcow2"
	}
	return filepath.Join(imagesDir, vm+"-"+dev+"."+ext)
}

// xmlEsc 转义 XML 属性/文本的五个保留字符。
func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", "\"", "&quot;")
	return r.Replace(s)
}
