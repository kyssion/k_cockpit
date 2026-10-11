// leases.go 与 sysfs.go：dnsmasq 租约解析与内核状态探测。
package sys

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Lease 是 dnsmasq 租约文件的一行。
type Lease struct {
	// ExpiresUnix 是到期时刻的 Unix 秒；0 表示永不过期。
	ExpiresUnix int64
	MAC         string
	IP          string
	Hostname    string
	ClientID    string
}

// ReadDHCPLeases 解析 libvirt dnsmasq 的租约文件（全部网络合并）。
//
// 行格式：<expiry> <mac> <ip> <hostname|*> <clientid|*>。
func ReadDHCPLeases(ctx context.Context) ([]Lease, error) {
	const dir = "/var/lib/libvirt/dnsmasq"
	entries, err := filepath.Glob(dir + "/*.leases")
	if err != nil || len(entries) == 0 {
		// 目录不存在或没有租约文件都是合法状态（还没有任何网络）。
		return nil, nil
	}
	var out []Lease
	for _, f := range entries {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		out = append(out, parseLeases(strings.Split(string(b), "\n"))...)
	}
	return out, nil
}

// parseLeases 解析租约行（星号占位转空串）。
func parseLeases(lines []string) []Lease {
	var out []Lease
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		expiry, _ := strconv.ParseInt(fields[0], 10, 64)
		hostname, clientid := fields[3], ""
		if len(fields) > 4 {
			clientid = fields[4]
		}
		if hostname == "*" {
			hostname = ""
		}
		if clientid == "*" {
			clientid = ""
		}
		out = append(out, Lease{
			ExpiresUnix: expiry, MAC: fields[1], IP: fields[2],
			Hostname: hostname, ClientID: clientid,
		})
	}
	return out
}

// ---- KSM ----

// KSMInfo 是 /sys/kernel/mm/ksm 的读数。
type KSMInfo struct {
	Enabled       bool
	RunMode       string
	PagesShared   int64
	PagesSharing  int64
	PagesUnshared int64
	FullScans     int64
}

// ReadKSM 读 KSM 状态。目录不存在（内核未编 KSM）返回 Enabled=false 的空值。
func ReadKSM() (*KSMInfo, error) {
	base := "/sys/kernel/mm/ksm"
	if _, err := os.Stat(base); err != nil {
		return &KSMInfo{}, nil
	}
	info := &KSMInfo{}
	if run, err := os.ReadFile(base + "/run"); err == nil {
		info.Enabled = strings.TrimSpace(string(run)) != "0"
	}
	if mode, err := os.ReadFile(base + "/merge_across_nodes"); err == nil {
		_ = mode
	}
	if advisory, err := os.ReadFile(base + "/advisory"); err == nil {
		info.RunMode = strings.TrimSpace(string(advisory))
	} else if run, err := os.ReadFile(base + "/run"); err == nil {
		switch strings.TrimSpace(string(run)) {
		case "0":
			info.RunMode = "stop"
		case "1":
			info.RunMode = "wake"
		default:
			info.RunMode = "always"
		}
	}
	readInt := func(name string) int64 {
		b, err := os.ReadFile(base + "/" + name)
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return v
	}
	info.PagesShared = readInt("pages_shared")
	info.PagesSharing = readInt("pages_sharing")
	info.PagesUnshared = readInt("pages_unshared")
	info.FullScans = readInt("full_scans")
	return info, nil
}

// ---- ZRAM ----

// ZRAMInfo 是 zram 设备的状态。
type ZRAMInfo struct {
	Enabled       bool
	DisksizeBytes int64
	UsedBytes     int64
	OrigDataBytes int64
	Algorithm     string
}

// ReadZRAM 读 zram 状态（读 sysfs 而不是解析 zramctl 表格：字段位置随
// 版本变化，sysfs 的键是稳定的）。
func ReadZRAM() (*ZRAMInfo, error) {
	info := &ZRAMInfo{}
	base := "/sys/block/zram0"
	if _, err := os.Stat(base); err != nil {
		return info, nil
	}
	info.Enabled = true
	readInt := func(name string) int64 {
		b, err := os.ReadFile(base + "/" + name)
		if err != nil {
			return 0
		}
		v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return v
	}
	info.DisksizeBytes = readInt("disksize")
	// mm_stat 的列（0 起）：orig_data_size, compr_data_size, mem_used_total…
	if b, err := os.ReadFile(base + "/mm_stat"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) >= 3 {
			info.OrigDataBytes, _ = strconv.ParseInt(fields[0], 10, 64)
			info.UsedBytes, _ = strconv.ParseInt(fields[2], 10, 64)
		}
	}
	if b, err := os.ReadFile(base + "/comp_algorithm"); err == nil {
		// 当前生效算法被 [方括号] 包着，如 "lzo [lz4] zstd"。
		for _, f := range strings.Fields(string(b)) {
			if strings.HasPrefix(f, "[") {
				info.Algorithm = strings.Trim(f, "[]")
			}
		}
	}
	return info, nil
}

// ---- 嵌套虚拟化 ----

// NestedInfo 是嵌套虚拟化的状态。
type NestedInfo struct {
	Enabled    bool
	Supported  bool
	Reason     string
	Persistent bool
	Fix        string
}

// ReadNested 读嵌套虚拟化状态。
//
// x86 走 kvm 模块参数（kvm_intel/kvm_amd 的 nested）；**aarch64 没有这个
// 参数**——ARM 上嵌套虚拟化由内核/硬件能力决定，当前 QEMU/ARM64 组合
// 通常不支持，如实报告「平台不支持」而不是编一个开关。
func ReadNested(ctx context.Context) (*NestedInfo, error) {
	if _, err := os.Stat("/sys/module/kvm_intel"); err == nil {
		return readNestedParam("/sys/module/kvm_intel/parameters/nested", "y",
			"echo 'options kvm_intel nested=Y' > /etc/modprobe.d/kvm-nested.conf 并重新加载 kvm_intel")
	}
	if _, err := os.Stat("/sys/module/kvm_amd"); err == nil {
		return readNestedParam("/sys/module/kvm_amd/parameters/nested", "1",
			"echo 'options kvm_amd nested=1' > /etc/modprobe.d/kvm-nested.conf 并重新加载 kvm_amd")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return &NestedInfo{Reason: "宿主机没有 /dev/kvm（KVM 未就绪）"}, nil
	}
	return &NestedInfo{
		Supported:  false,
		Reason:     "aarch64 平台当前不支持嵌套虚拟化（无 kvm 模块 nested 参数）",
		Persistent: true,
	}, nil
}

func readNestedParam(path, yes, fix string) (*NestedInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return &NestedInfo{Reason: "无法读取 " + path}, nil
	}
	v := strings.TrimSpace(string(b))
	enabled := v == "Y" || v == "y" || v == "1"
	return &NestedInfo{
		Enabled: enabled, Supported: true, Persistent: enabled,
		Fix: fix,
	}, nil
}

// ---- IOMMU / PCI ----

// IOMMUGroups 返回 IOMMU 分组目录里的组号清单；空切片=未启用 IOMMU。
func IOMMUGroups() []string {
	entries, err := os.ReadDir("/sys/kernel/iommu_groups")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// VFIOAvailable 报告 vfio-pci 驱动是否可用。
func VFIOAvailable() bool {
	if _, err := os.Stat("/sys/module/vfio_pci"); err == nil {
		return true
	}
	if _, err := os.Stat("/dev/vfio"); err == nil {
		return true
	}
	return RunOK(context.Background(), "modprobe", "vfio_pci")
}

// PCIDeviceLocal 是一台 PCI 设备（lspci 解析 + sysfs 补充）。
type PCIDeviceLocal struct {
	Address      string
	VendorDevice string // [vendor:device] 十六进制
	Class        string
	Description  string
	Driver       string
	IOMMUGroup   string
}

// ListPCIDevices 枚举 PCI 设备并补齐驱动与 IOMMU 分组。
//
// 直通的核心信息（分组）只在节点上能算：分组把「哪些设备必须一起直通」
// 固化在主板拓扑里，控制面无法推算。
func ListPCIDevices(ctx context.Context) ([]PCIDeviceLocal, error) {
	out, err := Run(ctx, "lspci", "-Dnnm")
	if err != nil {
		return nil, fmt.Errorf("lspci 不可用: %w", err)
	}
	var devices []PCIDeviceLocal
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		d := PCIDeviceLocal{Address: fields[0], Class: fields[1], Description: fields[2]}
		if len(fields) > 3 {
			// 形如 "Vendor [10de:1234]"，取方括号里的 id。
			if i := strings.Index(fields[3], "["); i >= 0 {
				d.VendorDevice = strings.Trim(fields[3][i:], "[]")
			}
		}
		d.Driver = pciDriver(d.Address)
		d.IOMMUGroup = pciIOMMUGroup(d.Address)
		devices = append(devices, d)
	}
	return devices, nil
}

// pciDriver 读设备当前绑定的驱动（vfio-pci / 内核原生驱动 / 空=无驱动）。
func pciDriver(addr string) string {
	link, err := os.Readlink("/sys/bus/pci/devices/" + addr + "/driver")
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

// pciIOMMUGroup 读设备的 IOMMU 分组号。
func pciIOMMUGroup(addr string) string {
	b, err := os.Readlink("/sys/bus/pci/devices/" + addr + "/iommu_group")
	if err != nil {
		return ""
	}
	return filepath.Base(b)
}

// ---- 连接表 ----

// ConnectionLocal 是一条已建立的 TCP 连接（ss 解析）。
type ConnectionLocal struct {
	RemoteAddr string
	LocalPort  int
	Process    string
}

// ListEstablishedConnections 读取入向已建立的 TCP 连接。
//
// `ss -tnp state established` 的行形如：
//
//	ESTAB 0 0 198.51.100.1:8443  198.51.100.10:52344 users:(("k_cockpit-agent",pid=1,fd=9))
//
// Local 在前、Peer 在后（-tn 默认视图）。只保留「对端地址 != 本机」的行
// 由调用方判断（Own 由控制面回填）。
func ListEstablishedConnections(ctx context.Context) ([]ConnectionLocal, error) {
	out, err := Run(ctx, "ss", "-tnp", "state", "established")
	if err != nil {
		return nil, err
	}
	conns, perr := parseEstablished(out)
	if perr != nil {
		return nil, perr
	}
	return conns, nil
}

// remoteOf 返回连接的对端地址（测试与展示用）。
func remoteOf(c ConnectionLocal) string { return c.RemoteAddr }

// parseEstablished 解析 ss -tnp state established 的输出。
func parseEstablished(out string) ([]ConnectionLocal, error) {
	var out_ []ConnectionLocal
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// 数据行的第 4 个字段是 Local、第 5 个是 Peer（表头行跳过）。
		if len(fields) < 5 || fields[0] != "ESTAB" {
			continue
		}
		local := fields[3]
		peer := fields[4]
		_, portStr, ok := strings.Cut(local, ":")
		if !ok {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		proc := ""
		if len(fields) > 5 && strings.HasPrefix(fields[5], "users:") {
			proc = extractProcess(fields[5])
		}
		out_ = append(out_, ConnectionLocal{
			RemoteAddr: peer, LocalPort: port, Process: proc,
		})
	}
	return out_, nil
}

// extractProcess 从 users:(("name",pid=1,fd=9)) 里取第一个进程名。
func extractProcess(s string) string {
	i := strings.Index(s, `"`)
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
