// ex_host.go：host.* 域的探测执行器。
package nodeagent

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// hostStats 实现 host.stats（F-8-01 的采样源）：宿主机累计计数。
//
// 四个流量字段是**自开机以来的累计字节数**——控制面的采集器按两次采样
// 的差算速率（见 HostStats 的契约注释），agent 每次只需如实报累计值。
// CPU 占用率仍是 1s 双采样的瞬时值。
func hostStats(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read_proc", Name: "读取 /proc 计数", Index: 1, Total: 2})
	a, err := sys.ReadCPUSample()
	if err != nil {
		return failResult("读取 /proc/stat 失败: " + err.Error()), nil
	}
	mem, err := sys.ReadMemInfo()
	if err != nil {
		return failResult("读取 /proc/meminfo 失败: " + err.Error()), nil
	}
	net, err := sys.ReadNetDevCounters()
	if err != nil {
		return failResult("读取网络计数失败: " + err.Error()), nil
	}
	disk, err := sys.ReadDiskStatsCounters(512)
	if err != nil {
		return failResult("读取磁盘计数失败: " + err.Error()), nil
	}
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	b, err := sys.ReadCPUSample()
	if err != nil {
		return failResult("读取 /proc/stat 失败: " + err.Error()), nil
	}

	report(Stage{Key: "devices", Name: "汇总设备计数", Index: 2, Total: 2})
	var netIn, netOut, diskRead, diskWrite int64
	devices := []agent.HostDeviceStat{}
	for name, c := range net {
		if name == "lo" {
			continue
		}
		netIn += c.ReadBytes
		netOut += c.WriteBytes
		devices = append(devices, agent.HostDeviceStat{
			Name: name, Kind: "net", ReadBytes: c.ReadBytes, WriteBytes: c.WriteBytes,
		})
	}
	for name, c := range disk {
		// 分区（sda1）与整盘（sda）都在 diskstats 里；整盘计数已含分区，
		// 只报整盘避免双算。
		if strings.ContainsAny(name, "0123456789") && !strings.HasPrefix(name, "nvme") {
			if _, isWhole := disk[trimDigits(name)]; isWhole {
				continue
			}
		}
		if c.ReadBytes == 0 && c.WriteBytes == 0 {
			continue
		}
		diskRead += c.ReadBytes
		diskWrite += c.WriteBytes
		devices = append(devices, agent.HostDeviceStat{
			Name: name, Kind: "disk", ReadBytes: c.ReadBytes, WriteBytes: c.WriteBytes,
		})
	}
	l1, l5, l15, _ := sys.ReadLoadAvg()
	uptime, _ := sys.ReadUptimeSeconds()
	devicesJSON, _ := json.Marshal(devices)

	return &agent.Result{Success: true, Data: map[string]any{
		agent.HostStatsDataKey: agent.HostStats{
			CPUPercent:     sys.CPUPercentBetween(a, b),
			CPUCores:       len(b.PerCoreTotal),
			Load1:          l1,
			Load5:          l5,
			Load15:         l15,
			MemUsedMB:      int64(mem.UsedMB),
			MemTotalMB:     int64(mem.TotalMB),
			NetInBytes:     netIn,
			NetOutBytes:    netOut,
			DiskReadBytes:  diskRead,
			DiskWriteBytes: diskWrite,
			Devices:        string(devicesJSON),
			UptimeSeconds:  uptime,
		},
	}}, nil
}

// trimDigits 去掉设备名尾部的分区号（sda1 → sda；nvme0n1p2 → nvme0n1）。
func trimDigits(name string) string {
	for len(name) > 0 && name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
		name = name[:len(name)-1]
	}
	// nvme 盘的分区是 p 结尾（nvme0n1p2）。
	if strings.HasSuffix(name, "p") {
		name = name[:len(name)-1]
	}
	return name
}

// hostHardware 实现 host.hardware（F-8-03 硬件详情）。
//
// MemSlot 依赖 SMBIOS（dmidecode），虚拟机里通常没有——空列表是「读不到」
// 的诚实表达（CPU 部分照常给出，Unavailable 只在**整个**探测失败时置位）。
func hostHardware(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "lscpu", Name: "读取 CPU 拓扑", Index: 1, Total: 2})
	out, err := sys.Run(ctx, "lscpu", "-J")
	if err != nil {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.HostHardwareDataKey: agent.HostHardware{Unavailable: "lscpu 不可用: " + err.Error()},
		}}, nil
	}
	cpuModel, sockets, coresPer, threads := parseLscpu(out)
	if cpuModel == "" {
		cpuModel = "未知"
	}

	report(Stage{Key: "cores", Name: "采样每核占用", Index: 2, Total: 2})
	a, err := sys.ReadCPUSample()
	if err != nil {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.HostHardwareDataKey: agent.HostHardware{Unavailable: "读取 /proc/stat 失败"},
		}}, nil
	}
	time.Sleep(300 * time.Millisecond)
	b, err := sys.ReadCPUSample()
	if err != nil {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.HostHardwareDataKey: agent.HostHardware{Unavailable: "读取 /proc/stat 失败"},
		}}, nil
	}
	corePercent := make([]float64, len(b.PerCoreTotal))
	for i := range corePercent {
		var dt, di uint64
		if i < len(a.PerCoreTotal) {
			dt = b.PerCoreTotal[i] - a.PerCoreTotal[i]
			di = b.PerCoreIdle[i] - a.PerCoreIdle[i]
		}
		if dt > 0 {
			corePercent[i] = float64(dt-di) / float64(dt) * 100
		}
	}

	return &agent.Result{Success: true, Data: map[string]any{
		agent.HostHardwareDataKey: agent.HostHardware{
			CPUModel:       cpuModel,
			Sockets:        sockets,
			CoresPerSocket: coresPer,
			ThreadsPerCore: threads,
			CorePercent:    corePercent,
			MemSlots:       []agent.MemSlot{}, // SMBIOS 不可得（虚拟机），空列表如实呈现
		},
	}}, nil
}

// parseLscpu 解析 lscpu -J 的关键字段。
func parseLscpu(out string) (model string, sockets, coresPer, threads int) {
	var data struct {
		Lscpu []struct {
			Field string `json:"field"`
			Data  string `json:"data"`
		} `json:"lscpu"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return "", 0, 0, 0
	}
	atoi := func(s string) int { v, _ := strconv.Atoi(strings.TrimSpace(s)); return v }
	for _, e := range data.Lscpu {
		switch e.Field {
		case "Model name:":
			model = strings.TrimSpace(e.Data)
		case "Socket(s):":
			sockets = atoi(e.Data)
		case "Core(s) per socket:", "Core(s) per cluster:":
			// x86 是 per socket；**aarch64 是 per cluster**（拓扑术语不同，
			// 语义等价：一颗物理单元里有多少核心）。
			coresPer = atoi(e.Data)
		case "Thread(s) per core:":
			threads = atoi(e.Data)
		}
	}
	if sockets == 0 {
		sockets = 1
	}
	if coresPer == 0 {
		coresPer = 1
	}
	if threads == 0 {
		threads = 1
	}
	return model, sockets, coresPer, threads
}

// hostNetstats 实现 host.netstats（规则与计数，非实时流量）。
func hostNetstats(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "rules", Name: "统计 NAT/DNAT 规则", Index: 1, Total: 2})
	natOut, err1 := sys.Run(ctx, "iptables", "-t", "nat", "-S")
	natRules, dnatRules := 0, 0
	if err1 == nil {
		for _, line := range strings.Split(natOut, "\n") {
			if strings.Contains(line, "-A") && strings.Contains(line, "MASQUERADE") {
				natRules++
			}
			if strings.Contains(line, "-A") && strings.Contains(line, "DNAT") {
				dnatRules++
			}
		}
	}

	report(Stage{Key: "bridges", Name: "汇总网桥计数", Index: 2, Total: 2})
	bridges, _ := sys.OVSListBridges(ctx)
	var ingress, egress int64
	stats := []agent.BridgeStat{}
	for _, br := range bridges {
		s := bridgeStat(br)
		ingress += s.RxBytes
		egress += s.TxBytes
		stats = append(stats, agent.BridgeStat{
			Name: br, RxBytes: s.RxBytes, TxBytes: s.TxBytes,
		})
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.HostNetStatsDataKey: agent.HostNetStats{
			NATRules:           natRules,
			SwitchIngressBytes: ingress,
			SwitchEgressBytes:  egress,
			DNATRules:          dnatRules,
			Bridges:            stats,
		},
	}}, nil
}

// bridgeStat 从 sysfs 读网桥的累计收发字节。
func bridgeStat(name string) agent.BridgeStat {
	s := agent.BridgeStat{Name: name}
	if b, err := os.ReadFile("/sys/class/net/" + name + "/statistics/rx_bytes"); err == nil {
		s.RxBytes, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	if b, err := os.ReadFile("/sys/class/net/" + name + "/statistics/tx_bytes"); err == nil {
		s.TxBytes, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	return s
}

// hostTuning 实现 host.tuning（KSM / ZRAM / 嵌套虚拟化状态）。
func hostTuning(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "sysfs", Name: "读取内核状态", Index: 1, Total: 1})
	ksm, _ := sys.ReadKSM()
	zram, _ := sys.ReadZRAM()
	nested, _ := sys.ReadNested(ctx)
	// KSM 的收益由节点算：页大小随架构变化（arm64 常见 64K），
	// 控制面猜错页大小的表现是一个看起来很有说服力的错误数字。
	ksmSaved := (ksm.PagesSharing - ksm.PagesShared) * int64(os.Getpagesize())
	return &agent.Result{Success: true, Data: map[string]any{
		agent.TuningStateKey: agent.TuningState{
			KSM: agent.KSMState{
				Enabled: ksm.Enabled, PagesShared: ksm.PagesShared,
				PagesSharing: ksm.PagesSharing, PagesUnshared: ksm.PagesUnshared,
				SavedBytes: ksmSaved, FullScans: ksm.FullScans, RunMode: ksm.RunMode,
			},
			ZRAM: agent.ZRAMState{
				Enabled: zram.Enabled, DisksizeBytes: zram.DisksizeBytes,
				UsedBytes: zram.UsedBytes, OrigDataBytes: zram.OrigDataBytes,
				Algorithm: zram.Algorithm,
			},
			Nested: agent.NestedState{
				Enabled: nested.Enabled, Supported: nested.Supported,
				Reason: nested.Reason, Persistent: nested.Persistent, Fix: nested.Fix,
			},
		},
	}}, nil
}

// hostIOMMU 实现 host.iommu（直通能力的前置探测）。
func hostIOMMU(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "iommu", Name: "检查 IOMMU 与 VFIO", Index: 1, Total: 1})
	groups := sys.IOMMUGroups()
	vfio := sys.VFIOAvailable()
	st := agent.IOMMUStatus{}
	switch {
	case len(groups) == 0:
		st.Reason = "内核未启用 IOMMU（/sys/kernel/iommu_groups 为空）"
		st.Fix = "在内核启动参数加 intel_iommu=on（或 amd_iommu=on）并重启宿主机"
		st.NeedReboot = true
	case !vfio:
		st.Reason = "IOMMU 已启用，但 vfio_pci 驱动不可用"
		st.Fix = "安装/加载 vfio-pci 模块（modprobe vfio_pci）"
	default:
		st.Enabled = true
		st.VFIOAvailable = true
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.IOMMUStatusKey: st}}, nil
}

// hostPCIDevices 实现 host.pci.devices（直通设备表）。
func hostPCIDevices(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "lspci", Name: "枚举 PCI 设备", Index: 1, Total: 2})
	devices, err := sys.ListPCIDevices(ctx)
	if err != nil {
		return failResult(err.Error()), nil
	}
	report(Stage{Key: "groups", Name: "解析 IOMMU 分组", Index: 2, Total: 2})
	groups := sys.IOMMUGroups()
	groupMembers := map[string][]string{}
	for _, g := range groups {
		entries, err := os.ReadDir("/sys/kernel/iommu_groups/" + g + "/devices")
		if err != nil {
			continue
		}
		for _, e := range entries {
			for _, d := range devices {
				if d.Address == e.Name() {
					continue
				}
				if dev, err := os.Readlink("/sys/bus/pci/devices/" + d.Address + "/iommu_group"); err == nil &&
					baseOf(dev) == g {
					groupMembers[d.Address] = append(groupMembers[d.Address], e.Name())
				}
			}
		}
	}

	out := make([]agent.PCIDevice, 0, len(devices))
	iommuOn := len(groups) > 0
	for _, d := range devices {
		group := 0
		if d.IOMMUGroup != "" {
			group, _ = strconv.Atoi(d.IOMMUGroup)
		}
		canPT := iommuOn && sys.VFIOAvailable() && d.Driver != "pcieport" && d.Driver != "pci_bridge"
		reason := ""
		if !canPT {
			switch {
			case !iommuOn:
				reason = "IOMMU 未启用"
			case !sys.VFIOAvailable():
				reason = "vfio_pci 不可用"
			default:
				reason = "桥接设备（跟随整组直通）"
			}
		}
		out = append(out, agent.PCIDevice{
			Address: d.Address, VendorDevice: d.VendorDevice,
			Class: d.Class, Description: d.Description,
			Driver: d.Driver, IOMUGroup: group,
			GroupMembers:   groupMembers[d.Address],
			CanPassthrough: canPT, Reason: reason,
		})
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.PCIDevicesKey: out}}, nil
}

// hostConnections 实现 host.connections（宿主机防火墙的连接表）。
func hostConnections(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "ss", Name: "读取连接表", Index: 1, Total: 1})
	conns, err := sys.ListEstablishedConnections(ctx)
	if err != nil {
		return failResult(err.Error()), nil
	}
	out := make([]agent.HostConnection, 0, len(conns))
	for _, c := range conns {
		out = append(out, agent.HostConnection{
			RemoteAddr: c.RemoteAddr, LocalPort: c.LocalPort,
			Protocol: "tcp", State: "ESTAB", Process: c.Process,
			// Own 由控制面回填（节点不知道请求方是谁）。
		})
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.HostConnectionsKey: out}}, nil
}

func baseOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
