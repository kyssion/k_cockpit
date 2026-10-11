// ex_node.go：node.* 域的探测执行器（node.stats / node.disks /
// node.network / node.domains）。
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// startedAt 是 agent 进程启动时刻（NodeStats.AgentStartedAt）。
var startedAt = time.Now()

// nodeStats 实现 node.stats（F-6-03）：宿主机瞬时指标。
//
// CPU 占用率在 handler 内做 1s 双采样：它是「瞬时」值而 /proc/stat 是
// 累计 tick，没有上一次采样就没有速率——有状态缓存会把「agent 重启后
// 第一次读数」变成需要特殊处理的路径，无状态双采样没有这个问题。
func nodeStats(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read_proc", Name: "读取 /proc 计数", Index: 1, Total: 2})
	a, err := sys.ReadCPUSample()
	if err != nil {
		return failResult("读取 /proc/stat 失败: " + err.Error()), nil
	}
	mem, err := sys.ReadMemInfo()
	if err != nil {
		return failResult("读取 /proc/meminfo 失败: " + err.Error()), nil
	}
	select {
	case <-time.After(time.Second): // 采样窗口
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	b, err := sys.ReadCPUSample()
	if err != nil {
		return failResult("读取 /proc/stat 失败: " + err.Error()), nil
	}

	report(Stage{Key: "read_storage", Name: "统计存储占用", Index: 2, Total: 2})
	diskTotal, diskUsed := storageUsage()

	l1, l5, l15, err := sys.ReadLoadAvg()
	if err != nil {
		return failResult("读取负载失败: " + err.Error()), nil
	}
	uptime, err := sys.ReadUptimeSeconds()
	if err != nil {
		uptime = 0
	}

	return &agent.Result{
		Success: true,
		Data: map[string]any{
			agent.NodeStatsDataKey: agent.NodeStats{
				CPUPercent:     sys.CPUPercentBetween(a, b),
				CPUCores:       len(b.PerCoreTotal),
				LoadAvg1:       l1,
				LoadAvg5:       l5,
				LoadAvg15:      l15,
				MemTotalMB:     mem.TotalMB,
				MemUsedMB:      mem.UsedMB,
				DiskTotalBytes: diskTotal,
				DiskUsedBytes:  diskUsed,
				UptimeSeconds:  uptime,
				AgentStartedAt: startedAt,
			},
		},
	}, nil
}

// storageUsage 返回数据盘用量。
//
// 语义是「所有存储池的合计」，但存储池注册在控制面、节点不知道路径
// （f-5-01 的池创建随 P4 落地后这里改为按池路径合计）。当前取数据根目录
// 所在文件系统——它与默认池位于同一块盘，量级正确。
func storageUsage() (total, used int64) {
	path := "/var/lib/k_cockpit"
	if _, err := os.Stat(path); err != nil {
		path = "/var/lib/libvirt/images"
	}
	if _, err := os.Stat(path); err != nil {
		path = "/"
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Blocks-st.Bfree) * int64(st.Bsize)
}

// nodeDisks 实现 node.disks：块设备清单（f-5-01 的选盘入口）。
func nodeDisks(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "lsblk", Name: "枚举块设备", Index: 1, Total: 2})
	devices, err := sys.ListBlockDevices(ctx)
	if err != nil {
		return failResult(err.Error()), nil
	}
	report(Stage{Key: "classify", Name: "判定系统盘与占用", Index: 2, Total: 2})
	rootSrc := rootMountSource(ctx)
	rootDev := sys.RootDevicePath(ctx)

	out := make([]agent.Disk, 0, len(devices))
	for _, d := range devices {
		if d.Type != "disk" {
			continue // 分区/loop/rom 不进选盘清单
		}
		isSystem := (rootDev != "" && sys.SamePath(d.Path, rootDev)) ||
			sys.SubtreeRootFS(d, rootSrc) || subtreeSwap(d)
		out = append(out, agent.Disk{
			DeviceID:   sys.StableDeviceID(d.Path),
			Path:       d.Path,
			SizeBytes:  parseSizeBytes(d.Size.String()),
			IsSystem:   isSystem,
			Mounted:    sys.SubtreeMounted(d),
			HasData:    sys.HasPartitionsOrFS(d),
			Filesystem: d.FSType,
			MountPoint: firstMountPoint(d),
		})
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.DiskListKey: out,
	}}, nil
}

// subtreeSwap 报告设备子树里是否有 swap（IsSystem 的另一个硬条件）。
func subtreeSwap(d sys.BlockDevice) bool {
	for _, c := range d.Children {
		if c.FSType == "swap" || c.MountPoint == "[SWAP]" {
			return true
		}
	}
	return false
}

// rootMountSource 返回根文件系统的源设备路径（findmnt）。
func rootMountSource(ctx context.Context) string {
	out, err := sys.Run(ctx, "findmnt", "-n", "-o", "SOURCE", "/")
	if err != nil {
		return ""
	}
	src := strings.TrimSpace(out)
	if i := strings.IndexByte(src, '['); i > 0 {
		src = src[:i] // btrfs 子卷后缀
	}
	return src
}

// firstMountPoint 返回设备子树里第一个挂载点（展示用）。
func firstMountPoint(d sys.BlockDevice) string {
	if d.MountPoint != "" {
		return d.MountPoint
	}
	for _, c := range d.Children {
		if mp := firstMountPoint(c); mp != "" {
			return mp
		}
	}
	return ""
}

// parseSizeBytes 把 lsblk 的人类可读容量（"128G"）换算成字节。
// -b 模式下 lsblk 给纯数字，这里两种都兼容。
func parseSizeBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var n float64
	unit := byte(0)
	if _, err := fmt.Sscanf(s, "%f%c", &n, &unit); err != nil {
		// 纯数字（-b 模式）。
		var v int64
		if _, err := fmt.Sscanf(s, "%d", &v); err == nil {
			return v
		}
		return 0
	}
	mult := float64(1)
	switch unit {
	case 'K':
		mult = 1024
	case 'M':
		mult = 1024 * 1024
	case 'G':
		mult = 1024 * 1024 * 1024
	case 'T':
		mult = 1024 * 1024 * 1024 * 1024
	}
	return int64(n * mult)
}

// nodeNetwork 实现 node.network（f-4-01 R-003 能力自报）。
func nodeNetwork(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "probe", Name: "探测网络组件", Index: 1, Total: 1})

	backend := agent.NetworkBackend{Mode: agent.ModeBasic, Capabilities: []string{}, Missing: map[string]string{}}
	// 基础网桥：ip link 能用就有（内核必备功能）。
	if sys.RunOK(ctx, "ip", "link", "help") {
		backend.Capabilities = append(backend.Capabilities, agent.CapabilityBridgeBasic)
	} else {
		backend.Missing[agent.CapabilityBridgeBasic] = "ip 命令不可用"
	}
	// DHCP：dnsmasq 在不在 PATH 上。
	if sys.RunOK(ctx, "dnsmasq", "--help") {
		backend.Capabilities = append(backend.Capabilities, agent.CapabilityDHCP)
	} else {
		backend.Missing[agent.CapabilityDHCP] = "未安装 dnsmasq"
	}
	// OVS：缺失不阻断服务（R-004），只失去 VPC 进阶能力。
	if v, _ := sys.OVSVersion(ctx); v != "" {
		backend.Capabilities = append(backend.Capabilities, agent.CapabilityOVS)
	} else {
		backend.Missing[agent.CapabilityOVS] = "未安装 Open vSwitch"
	}
	// NAT：nft/iptables 后端任一可用即可。
	if sys.RunOK(ctx, "iptables", "--version") || sys.RunOK(ctx, "nft", "--version") {
		backend.Capabilities = append(backend.Capabilities, agent.CapabilityNAT)
	} else {
		backend.Missing[agent.CapabilityNAT] = "iptables 与 nft 均不可用"
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.NetworkKey: backend}}, nil
}

// nodeDomains 实现 node.domains（存量纳管的扫描来源）。
//
// **不过滤已纳管**：「哪些域有投影」是控制面的账，节点返回全部。
func nodeDomains(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	entries, err := sys.ListDomains(ctx)
	if err != nil {
		return failResult("virsh list 失败: " + err.Error()), nil
	}
	out := make([]agent.DomainInfo, 0, len(entries))
	for _, e := range entries {
		info := agent.DomainInfo{
			Name:  e.Name,
			State: sys.DomainState(e.State),
		}
		// 规格（vCPU/内存/磁盘）从定义读取；读不到的域（定义损坏）跳过
		// 细节、保留名字与状态——扫描不应因一个坏域整体失败。
		if xml, xerr := sys.DumpXML(ctx, e.Name, false); xerr == nil {
			info.VCPU = xmlInt(xml, "<vcpu")
			info.MemoryMB = xmlMemoryKiB(xml) / 1024
			info.DiskGB = 0
			if disks := sys.ParseDomainDisks(xml); len(disks) > 0 {
				var totalBytes int64
				for _, d := range disks {
					if d.IsCDROM || d.IsFloppy || d.SourceFile == "" {
						continue
					}
					if sz, serr := sys.QemuImgSize(ctx, d.SourceFile); serr == nil {
						totalBytes += sz
					}
				}
				info.DiskGB = int(totalBytes / (1024 * 1024 * 1024))
			}
		}
		info.Autostart = domainAutostart(ctx, e.Name)
		out = append(out, info)
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.NodeDomainsDataKey: out}}, nil
}

// domainAutostart 读域的自启标记（virsh dominfo 的 Autostart 行）。
func domainAutostart(ctx context.Context, name string) bool {
	out, err := sys.Run(ctx, "virsh", "-r", "-c", "qemu:///system", "dominfo", name)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Autostart:") {
			return strings.Contains(line, "enable")
		}
	}
	return false
}

// xmlInt 从 XML 里取 `<vcpu N>` 这类「元素文本是数字」的值。
func xmlInt(xml, tag string) int {
	i := strings.Index(xml, tag)
	if i < 0 {
		return 0
	}
	rest := xml[i:]
	if gt := strings.IndexByte(rest, '>'); gt >= 0 {
		rest = rest[gt+1:]
		if lt := strings.IndexByte(rest, '<'); lt >= 0 {
			v := strings.TrimSpace(rest[:lt])
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// xmlMemoryKiB 取 <memory unit='KiB'>N</memory>。
func xmlMemoryKiB(xml string) int {
	return xmlInt(xml, "<memory")
}

// jsonOf 调试用的小助手（保持 import 对齐）。
var _ = json.Marshal
