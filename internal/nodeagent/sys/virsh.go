// virsh.go：libvirt 探测的命令行封装。
//
// P1 的探测走 `virsh -r`（只读连接，系统实例）而不是 libvirt 客户端库：
// 探测命令的输出格式稳定、可用真机 fixture 单测；libvirt 客户端库随 P2
// 生命周期操作引入（define/undefine/snapshot 在那里收益最大），届时本文件
// 的解析器保留为命令降级路径（AGENTS §2：libvirt 客户端优先，命令降级）。
package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// virshBase 是只读系统实例连接的前缀。agent 以 root 运行，-r 足够。
var virshBase = []string{"-r", "-c", "qemu:///system"}

func virsh(ctx context.Context, args ...string) (string, error) {
	return Run(ctx, "virsh", append(append([]string{}, virshBase...), args...)...)
}

// DomainEntry 是 virsh list --all 的一行。
type DomainEntry struct {
	Name  string
	State string // libvirt 原始文案（running / shut off / paused …）
	ID    int    // 运行中的域才有 id，关机为 -1
}

// ListDomains 列出系统实例的全部域（含关机）。
func ListDomains(ctx context.Context) ([]DomainEntry, error) {
	out, err := virsh(ctx, "list", "--all")
	if err != nil {
		return nil, err
	}
	return parseVirshList(out), nil
}

// parseVirshList 解析 virsh list --all 的表格式输出。
//
// libvirt 用空格对齐表格，名称里可能含空格？——不允许：libvirt 域名
// 规范禁止空白，因此按字段切分是安全的。
func parseVirshList(out string) []DomainEntry {
	var out_ []DomainEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Id ") {
			continue
		}
		// 分隔线是「只含 - 与空格」的行；关机域的数据行同样以 "-" 开头
		// （Id 列为 "-"），不能按前缀混为一谈。
		if strings.Trim(line, "- ") == "" {
			continue
		}
		// libvirt 域名不含空白，因此按字段切分安全；状态可能多词
		//（"shut off"），用 Join 还原。
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		id := -1
		if v, err := strconv.Atoi(fields[0]); err == nil {
			id = v
		}
		out_ = append(out_, DomainEntry{
			Name:  fields[1],
			State: strings.Join(fields[2:], " "),
			ID:    id,
		})
	}
	return out_
}

// DomainState 把 libvirt 的状态文案映射为控制面的状态值
// （model.VMStatus 的取值域；映射表与 mock/控制面约定一致）。
func DomainState(libvirtState string) string {
	switch strings.ToLower(strings.TrimSpace(libvirtState)) {
	case "running":
		return "running"
	case "shut off", "shutoff":
		return "stopped"
	case "paused":
		return "paused"
	case "pmsuspended", "suspended":
		return "suspended"
	case "crashed":
		return "error"
	default:
		return "unknown"
	}
}

// DomainStateOf 查询单个域的状态（控制面状态值）。
func DomainStateOf(ctx context.Context, name string) (string, error) {
	out, err := virsh(ctx, "domstate", name)
	if err != nil {
		return "", err
	}
	return DomainState(out), nil
}

// DomainExists 报告域是否存在于系统实例（含关机）。
func DomainExists(ctx context.Context, name string) bool {
	return RunOK(ctx, "virsh", append(append([]string{}, virshBase...), "dominfo", name)...)
}

// DumpXML 读取域定义。live=true 取运行中定义（含热插拔后的实际状态），
// false 取持久定义（--inactive）。
func DumpXML(ctx context.Context, name string, live bool) (string, error) {
	args := []string{"dumpxml"}
	if !live {
		args = append(args, "--inactive")
	}
	args = append(args, name)
	return virsh(ctx, args...)
}

// DomainUUID 查询域的 libvirt UUID。
func DomainUUID(ctx context.Context, name string) (string, error) {
	out, err := virsh(ctx, "domuuid", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// DomStats 是 `virsh domstats` 的一次采样（只保留需要的累计计数）。
type DomStats struct {
	// CPUTimeNS 是域自启动以来消耗的 CPU 时间（纳秒）。
	CPUTimeNS uint64
	// VCPUs 是当前配置的 vCPU 数（0 表示未运行或未上报）。
	VCPUs int
	// MemTotalMB / MemUsedMB 来自 balloon（当前气球内存即来宾可见内存）。
	MemTotalMB int
	MemUsedMB  int
	// NetRxBytes / NetTxBytes 是全部网口的累计字节。
	NetRxBytes int64
	NetTxBytes int64
	// DiskReadBytes / DiskWriteBytes 是全部磁盘的累计读写。
	DiskReadBytes  int64
	DiskWriteBytes int64
	// DirtyRateMBps 是脏页速率（需要预先触发计算；0 表示没有读数）。
	DirtyRateMBps uint64
	Running       bool
}

// DomainStats 采样一个域的累计计数。
func DomainStats(ctx context.Context, name string) (*DomStats, error) {
	out, err := virsh(ctx, "domstats", name,
		"--cpu-total", "--balloon", "--interface", "--block", "--dirtyrate")
	if err != nil {
		return nil, err
	}
	return parseDomStats(out), nil
}

// parseDomStats 解析 domstats 的 key=value 行格式。
func parseDomStats(out string) *DomStats {
	s := &DomStats{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		v = strings.Trim(v, "\"")
		// 行首是 "域名: " 前缀（virsh 的输出样式），剥掉再匹配后缀。
		if i := strings.Index(k, ": "); i >= 0 {
			k = strings.TrimSpace(k[i+2:])
		}
		// 剥掉 "域名: " 前缀后，键形如 cpu.time / vcpu.current /
		// net.0.rx.bytes / block.0.rd.bytes（per-device 键以设备序号分段）。
		switch {
		case k == "cpu.time":
			s.CPUTimeNS = parseUint(v)
		case k == "vcpu.current":
			s.VCPUs = int(parseUint(v))
		case k == "balloon.current":
			s.MemTotalMB = int(parseUint(v)) / 1024
		case strings.HasPrefix(k, "net.") && strings.HasSuffix(k, ".rx.bytes"):
			s.NetRxBytes += int64(parseUint(v))
		case strings.HasPrefix(k, "net.") && strings.HasSuffix(k, ".tx.bytes"):
			s.NetTxBytes += int64(parseUint(v))
		case strings.HasPrefix(k, "block.") && strings.HasSuffix(k, ".rd.bytes"):
			s.DiskReadBytes += int64(parseUint(v))
		case strings.HasPrefix(k, "block.") && strings.HasSuffix(k, ".wr.bytes"):
			s.DiskWriteBytes += int64(parseUint(v))
		case k == "dirtyrate.calc":
			s.DirtyRateMBps = parseUint(v)
		}
	}
	// balloon.current 是来宾可见的内存（KiB）；「来宾内部实际用了多少」
	// 需要 Guest Agent（P5 来宾自动化接入），因此 UsedMB 留 0——执行器
	// 会以宿主侧口径补充说明而不是编一个数字。
	s.Running = s.CPUTimeNS > 0 || s.VCPUs > 0
	return s
}

func parseUint(v string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
	return n
}

// DiskXML 是 dumpxml 里解析出的一块磁盘。
type DiskXML struct {
	Dev        string // 目标设备名（vda / sdb…）
	Bus        string
	SourceFile string
	Format     string
	IsCDROM    bool
	IsFloppy   bool
	Device     string // disk / cdrom / floppy
}

// ParseDomainDisks 从 dumpxml 提取磁盘设备。
//
// 只认 <disk type='file|block'> 的 <target dev>/<bus> 与 <source file>；
// cdrom/floppy 单独标记（执行器据此跳过容量计算）。
func ParseDomainDisks(xml string) []DiskXML {
	var out []DiskXML
	for _, seg := range splitElements(xml, "<disk ", "</disk>") {
		d := DiskXML{
			Dev:      attrOf(seg, "<target", "dev"),
			Bus:      attrOf(seg, "<target", "bus"),
			Format:   attrOf(seg, "<driver", "type"),
			Device:   attrOf(seg, "<disk ", "device"),
			IsCDROM:  attrOf(seg, "<disk ", "device") == "cdrom",
			IsFloppy: attrOf(seg, "<disk ", "device") == "floppy",
		}
		src := attrOf(seg, "<source", "file")
		if src == "" {
			src = attrOf(seg, "<source", "dev")
		}
		d.SourceFile = src
		if d.Dev != "" {
			out = append(out, d)
		}
	}
	return out
}

// MachineTypeOf 从 dumpxml 提取机型（q35 / i440fx / virt）。
func MachineTypeOf(xml string) string {
	return attrOf(xml, "<type", "machine")
}

// splitElements 把 xml 按「起始标记 → 结束标记」切块（足够处理 libvirt
// 的规则输出；完整的 XML 解析交给 encoding/xml 的场景在 P2）。
func splitElements(xml, startTag, endTag string) []string {
	var out []string
	for {
		i := strings.Index(xml, startTag)
		if i < 0 {
			return out
		}
		j := strings.Index(xml[i:], endTag)
		if j < 0 {
			return out
		}
		out = append(out, xml[i:i+j+len(endTag)])
		xml = xml[i+j+len(endTag):]
	}
}

// attrOf 在含 tag 的那一行里取属性值。
//
// libvirt dumpxml 的属性用**单引号**（type='kvm'），而用户手写的片段可能
// 是双引号——两种都认，取先出现的那个引号样式。
func attrOf(xml, tag, attr string) string {
	i := strings.Index(xml, tag)
	if i < 0 {
		return ""
	}
	lineEnd := strings.IndexByte(xml[i:], '>')
	if lineEnd < 0 {
		return ""
	}
	line := xml[i : i+lineEnd]
	for _, q := range []byte{'\'', '"'} {
		key := attr + "=" + string(q)
		j := strings.Index(line, key)
		if j < 0 {
			continue
		}
		rest := line[j+len(key):]
		k := strings.IndexByte(rest, q)
		if k < 0 {
			return ""
		}
		return rest[:k]
	}
	return ""
}

// QemuImgSize 用 qemu-img info 读取镜像的虚拟容量（字节）。
//
// 必须用**顶层键解码**而不是字符串扫描：qemu ≥ 6.1 的 JSON 里带 children
// 链（backing file 信息），每个子节点也有自己的 "virtual-size"——扫描会
// 取到子节点的 192KB，容量显示为 0 的 bug 就是这么来的。
func QemuImgSize(ctx context.Context, path string) (int64, error) {
	out, err := Run(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return 0, err
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return 0, fmt.Errorf("qemu-img 输出解析失败: %w", err)
	}
	if info.VirtualSize == 0 {
		return 0, fmt.Errorf("qemu-img 输出缺少 virtual-size")
	}
	return info.VirtualSize, nil
}
