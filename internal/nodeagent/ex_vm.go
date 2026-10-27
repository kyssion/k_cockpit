// ex_vm.go：vm.* 域的只读探测执行器。
package nodeagent

import (
	"context"
	"os"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// vmStatus 实现 vm.status：写操作受理前的实时状态探测（f-2-01 R-002）。
func vmStatus(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "domstate", Name: "查询域状态", Index: 1, Total: 1})
	state, err := sys.DomainStateOf(ctx, op.Target)
	if err != nil {
		// 域不存在也是一次成功往返：状态 unknown 让控制面按「虚拟化层里
		// 没有这台机器」处理（与投影漂移的语义一致）。
		// 英文/中文 locale 两套 virsh 报错都认（真机踩过：英文环境报
		// "failed to get domain"，未映射时把「域不存在」当成了探测失败）。
		if strings.Contains(err.Error(), "not a valid") ||
			strings.Contains(err.Error(), "failed to get domain") ||
			strings.Contains(err.Error(), "失败的") {
			return &agent.Result{Success: true, Data: map[string]any{
				agent.StatusDataKey: "unknown",
			}}, nil
		}
		return failResult("virsh domstate 失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.StatusDataKey: state,
	}}, nil
}

// vmStats 实现 vm.stats（Hero 资源卡 + 采集器）。
//
// 网络与磁盘吞吐由 **1s 双采样**的 domstats 差分得到（domstats 给的是
// 累计字节）；CPU 按全部 vCPU 归一。MemUsed 需要来宾内视角（balloon 只给
// 配置值），留 0——前端对 0 的呈现是「—」，比编一个宿主侧近似诚实。
func vmStats(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "sample_first", Name: "第一次采样", Index: 1, Total: 2})
	a, err := sys.DomainStats(ctx, op.Target)
	if err != nil {
		return failResult("domstats 失败: " + err.Error()), nil
	}
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	report(Stage{Key: "sample_second", Name: "第二次采样并计算", Index: 2, Total: 2})
	b, err := sys.DomainStats(ctx, op.Target)
	if err != nil {
		return failResult("domstats 失败: " + err.Error()), nil
	}

	stats := agent.VMStats{UptimeSeconds: 0}
	if b.Running && b.VCPUs > 0 {
		// Δ(cpu.time ns) / 1e9 = 期间用掉的核·秒；除以 vCPU 数再归一到百分比。
		usedCoreSeconds := float64(b.CPUTimeNS-a.CPUTimeNS) / 1e9
		stats.CPUPercent = usedCoreSeconds / float64(b.VCPUs) * 100
		stats.NetRxKbps = float64(b.NetRxBytes-a.NetRxBytes) * 8 / 1024
		stats.NetTxKbps = float64(b.NetTxBytes-a.NetTxBytes) * 8 / 1024
		stats.DiskReadKbps = float64(b.DiskReadBytes-a.DiskReadBytes) / 1024
		stats.DiskWriteKbps = float64(b.DiskWriteBytes-a.DiskWriteBytes) / 1024
		stats.MemTotalMB = b.MemTotalMB
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.StatsDataKey: stats}}, nil
}

// vmDiskList 实现 vm.disk.list（F-2-06 磁盘页签）。
func vmDiskList(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "dumpxml", Name: "读取域定义", Index: 1, Total: 2})
	xml, err := sys.DumpXML(ctx, op.Target, true)
	if err != nil {
		return failResult("dumpxml 失败: " + err.Error()), nil
	}
	report(Stage{Key: "inspect", Name: "探测磁盘容量", Index: 2, Total: 2})
	disks := sys.ParseDomainDisks(xml)
	out := make([]agent.VMDisk, 0, len(disks))
	for _, d := range disks {
		if d.IsCDROM || d.IsFloppy {
			continue // 光驱/软驱有自己的页签，不属于磁盘清单
		}
		vd := agent.VMDisk{
			Dev: d.Dev, Bus: d.Bus, Format: d.Format, Source: d.SourceFile,
			// 系统盘判定按设备序号：vda/sda 是创建时的第一块盘。这是
			// **启发式**——真正的系统盘语义在 P2 随创建流程固化。
			IsSystem:     d.Dev == "vda" || d.Dev == "sda",
			Hotpluggable: !sysIsSystemDev(d.Dev),
		}
		if d.SourceFile != "" {
			if sz, serr := sys.QemuImgSize(ctx, d.SourceFile); serr == nil {
				vd.CapacityGB = int(sz / (1024 * 1024 * 1024))
			}
			if st, serr := os.Stat(d.SourceFile); serr == nil {
				vd.ActualBytes = st.Size()
			}
		}
		out = append(out, vd)
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.VMDiskListDataKey: out}}, nil
}

// sysIsSystemDev 系统盘不可卸载（与磁盘页签的约束一致）。
func sysIsSystemDev(dev string) bool { return dev == "vda" || dev == "sda" }

// vmXML 实现 vm.xml：**原样**返回域定义（脱敏在控制面做，见 VMXMLInfo 注释）。
func vmXML(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "dumpxml", Name: "读取域定义", Index: 1, Total: 1})
	live := true
	if v, ok := op.Params["live"].(bool); ok {
		live = v
	}
	xml, err := sys.DumpXML(ctx, op.Target, live)
	if err != nil {
		return failResult("dumpxml 失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.VMXMLKey: agent.VMXMLInfo{XML: xml, Live: live},
	}}, nil
}

// vmPCIeInfo 实现 vm.pcie.info（热插槽位余量）。
//
// P1 采用启发式口径：总槽位取 pcie-root-port 控制器数（q35/virt 标准 14），
// 已用 = 非 root 总线的 PCI 设备地址数；精化随 P2 的创建流程（槽位由
// 创建方分配并记账）。
func vmPCIeInfo(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "dumpxml", Name: "解析机型与槽位", Index: 1, Total: 1})
	xml, err := sys.DumpXML(ctx, op.Target, true)
	if err != nil {
		return failResult("dumpxml 失败: " + err.Error()), nil
	}
	machine := sys.MachineTypeOf(xml)
	total := strings.Count(xml, "model='pcie-root-port'")
	used := strings.Count(xml, "<address type='pci'")

	info := agent.PCIeInfo{MachineType: machine}
	// 机型带版本号（virt-8.2 / q35-7.2），按前缀匹配家族。
	isPCIe := strings.HasPrefix(machine, "q35") || strings.HasPrefix(machine, "virt")
	if isPCIe {
		info.HotplugSupported = true
		if total == 0 {
			total = 14 // 创建器未显式布置根端口时的标准数量（q35/virt）
		}
		info.Total = total
		if used > total {
			used = total
		}
		info.Free = total - used
	} else {
		info.HotplugSupported = false
		info.Reason = "机型 " + machine + " 无 PCIe 根端口，不支持设备热插拔"
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.PCIeDataKey: info}}, nil
}

// vmNeighbor 实现 vm.neighbor：这台机器所在二层网络能看见谁。
//
// 过滤口径：只返回**这台虚拟机的网口**上学到的邻居——全表返回会把宿主机
// 自己的邻居混进来，而那是控制面无法替节点过滤的。
func vmNeighbor(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "interfaces", Name: "解析虚拟机网口", Index: 1, Total: 2})
	xml, err := sys.DumpXML(ctx, op.Target, true)
	if err != nil {
		return failResult("dumpxml 失败: " + err.Error()), nil
	}
	vnets := parseVnetTargets(xml)

	report(Stage{Key: "neighbors", Name: "读取邻居表", Index: 2, Total: 2})
	entries, err := sys.ListNeighbors(ctx)
	if err != nil {
		return failResult("读取邻居表失败: " + err.Error()), nil
	}
	out := []agent.NeighborEntry{}
	for _, e := range entries {
		if len(vnets) > 0 && !vnets[e.Interface] {
			continue
		}
		out = append(out, agent.NeighborEntry{
			IP: e.IP, MAC: e.MAC, Interface: e.Interface, State: e.State,
			Bridge: sys.BridgeOfInterface(ctx, e.Interface),
		})
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.NeighborDataKey: out}}, nil
}

// parseVnetTargets 从 dumpxml 提取虚拟机网口的 target dev（vnetN）。
func parseVnetTargets(xml string) map[string]bool {
	out := map[string]bool{}
	for _, seg := range strings.Split(xml, "<interface ") {
		if len(out) > 0 || strings.Contains(seg, "</interface>") {
			// 只在切段后查找 target
		}
		if dev := attrDevOf(seg); dev != "" {
			out[dev] = true
		}
	}
	return out
}

// attrDevOf 取一个 interface 切段里的 target dev。
func attrDevOf(seg string) string {
	i := strings.Index(seg, "<target")
	if i < 0 {
		return ""
	}
	lineEnd := strings.IndexByte(seg[i:], '>')
	if lineEnd < 0 {
		return ""
	}
	line := seg[i : i+lineEnd]
	for _, q := range []byte{'\'', '"'} {
		key := "dev=" + string(q)
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
