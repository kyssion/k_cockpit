// ex_net.go：网络探测执行器（network.probe / ovs.* / dhcp.leases /
// platform.check / vm.migrate.assess）。
package nodeagent

import (
	"context"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// networkProbe 实现 network.probe（f-4-01 的能力探测，可随时重问）。
func networkProbe(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "ovs", Name: "探测 OVS", Index: 1, Total: 3})
	cap := agent.NetworkCapability{KernelModules: []string{}, UplinkCandidates: []agent.UplinkCandidate{}, Notes: []string{}}

	if v, _ := sys.OVSVersion(ctx); v != "" {
		cap.OVSAvailable = true
		cap.OVSVersion = v
	} else {
		cap.Notes = append(cap.Notes, "未安装 Open vSwitch，VPC 相关能力将降级到 Linux 网桥")
	}
	if modules, _ := sys.Run(ctx, "sh", "-c", "lsmod | grep -E '^(openvswitch|bridge|brnetfilter)' | awk '{print $1}'"); modules != "" {
		for _, m := range strings.Split(strings.TrimSpace(modules), "\n") {
			if m != "" {
				cap.KernelModules = append(cap.KernelModules, m)
			}
		}
	}

	report(Stage{Key: "uplinks", Name: "枚举上行口候选", Index: 2, Total: 3})
	addrs, err := sys.ListAddrs(ctx)
	if err != nil {
		return failResult("枚举网口失败: " + err.Error()), nil
	}
	links, err := sys.ListLinks(ctx)
	if err != nil {
		return failResult("枚举网口失败: " + err.Error()), nil
	}
	bridgePorts := map[string]bool{}
	if brs, _ := sys.OVSListBridges(ctx); brs != nil {
		for _, br := range brs {
			bridgePorts[br] = true
			for _, p := range mustPorts(ctx, br) {
				bridgePorts[p] = true
			}
		}
	}
	for _, l := range links {
		if !sys.IsPhysicalLike(l.IfName) || bridgePorts[l.IfName] {
			continue
		}
		up := false
		for _, f := range l.Flags {
			if f == "UP" {
				up = true
			}
		}
		cap.UplinkCandidates = append(cap.UplinkCandidates, agent.UplinkCandidate{
			Name:  l.IfName,
			Up:    up,
			HasIP: sys.LinkHasIP(addrs, l.IfName),
			Speed: sys.LinkSpeedMbps(ctx, l.IfName),
		})
	}

	report(Stage{Key: "notes", Name: "汇总", Index: 3, Total: 3})
	return &agent.Result{Success: true, Data: map[string]any{agent.NetworkCapabilityKey: cap}}, nil
}

func mustPorts(ctx context.Context, br string) []string {
	ps, _ := sys.OVSPortsOfBridge(ctx, br)
	return ps
}

// ovsStatus 实现 ovs.status：「装的那份现在健康吗」（与 network.probe 分工）。
func ovsStatus(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "service", Name: "检查服务与版本", Index: 1, Total: 2})
	st := agent.OVSStatus{}
	version, _ := sys.OVSVersion(ctx)
	if version == "" {
		st.Reason = "未安装 Open vSwitch"
		st.Fix = "安装 openvswitch-switch 包后重试"
		return &agent.Result{Success: true, Data: map[string]any{agent.OVSStatusKey: st}}, nil
	}
	st.Version = version
	st.ServiceActive = sys.OVSRunning(ctx)
	if !st.ServiceActive {
		st.Reason = "ovs-vswitchd 服务未运行"
		st.Fix = "systemctl start openvswitch-switch"
		return &agent.Result{Success: true, Data: map[string]any{agent.OVSStatusKey: st}}, nil
	}

	report(Stage{Key: "scale", Name: "统计规模", Index: 2, Total: 2})
	st.Available = true
	st.OpenFlow13 = sys.RunOK(ctx, "ovs-ofctl", "-O", "OpenFlow13", "show", "br-ovs") ||
		sys.RunOK(ctx, "ovs-ofctl", "-O", "OpenFlow13", "show", "br0")
	if brs, _ := sys.OVSListBridges(ctx); len(brs) > 0 {
		st.BridgeCount = len(brs)
		flows := 0
		for _, br := range brs {
			ps, _ := sys.OVSPortsOfBridge(ctx, br)
			st.PortCount += len(ps)
			if n, ok := sys.OVSFlowCount(ctx, br); ok {
				st.OpenFlow13 = true
				flows += n
				if sys.OVSMeterAvailable(ctx, br) {
					st.MeterAvailable = true
				}
			}
		}
		st.FlowCount = flows
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.OVSStatusKey: st}}, nil
}

// ovsPorts 实现 ovs.ports（虚拟机网口挂载的排查入口）。
func ovsPorts(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "ports", Name: "枚举桥与端口", Index: 1, Total: 1})
	brs, _ := sys.OVSListBridges(ctx)
	out := []agent.OVSPort{}
	for _, br := range brs {
		ports, err := sys.OVSPortsOfBridge(ctx, br)
		if err != nil {
			continue
		}
		for _, p := range ports {
			typ, tag := sys.OVSPortInfo(ctx, br, p)
			out = append(out, agent.OVSPort{Name: p, Bridge: br, Type: typ, Tag: tag})
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.OVSPortsKey: out}}, nil
}

// dhcpLeases 实现 dhcp.leases（IP 冲突排查的入口）。
func dhcpLeases(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "leases", Name: "读取租约文件", Index: 1, Total: 1})
	leases, err := sys.ReadDHCPLeases(ctx)
	if err != nil {
		return failResult(err.Error()), nil
	}
	out := make([]agent.DHCPLease, 0, len(leases))
	for _, l := range leases {
		expiry := ""
		if l.ExpiresUnix > 0 {
			expiry = time.Unix(l.ExpiresUnix, 0).Format(time.RFC3339)
		}
		out = append(out, agent.DHCPLease{
			ExpiresAt: expiry, MAC: l.MAC, IP: l.IP,
			Hostname: l.Hostname, ClientID: l.ClientID,
		})
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.DHCPLeasesKey: out}}, nil
}

// platformCheck 实现 platform.check（期望状态 vs 实际状态）。
//
// 逐项核对的前提是节点能按 Target 定位对象：端口安全有网口名、端口镜像
// 有镜像名（节点侧 Mirror 行名为 kc-mir-<name>）。公网 IP 绑定只带控制面
// 记录 id，节点规则却按公网 IP 登记，无从定位——这类项标记 Unverifiable，
// 由控制面单独计数，避免「查不了」被当成「配置丢了」的恒定假偏差。
func platformCheck(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "check", Name: "逐项核对", Index: 1, Total: 1})
	var expectations []agent.PlatformExpectation
	if raw, ok := op.Params["expectations"]; ok {
		_ = decodeExpectations(raw, &expectations)
	}
	results := make([]agent.PlatformCheckResult, 0, len(expectations))
	for _, e := range expectations {
		r := agent.PlatformCheckResult{ID: e.ID}
		switch e.Kind {
		case "port_security":
			// Target 是网口引用：能确认 OVS 上是否有这个口。
			if _, err := sys.OVSInterfaceBridge(ctx, e.Target); err == nil {
				r.Present = true
				r.Actual = "端口存在于 OVS（流表深度校验尚未接入）"
			} else {
				r.Actual = "OVS 上没有该端口"
			}
		case "port_mirror":
			// 多目标镜像一个目标一条 Mirror 行（kc-mir-<name>-<i>），因此
			// 不能只按旧版单行名判定——那样已生效的镜像会被报成「配置丢了」，
			// 自检就变成了恒定的假偏差。
			if row, ok := mirrorPresentRow(ctx, e.Target); ok {
				r.Present = true
				r.Actual = "OVS 上存在镜像行 " + row
			} else {
				r.Actual = "OVS 上没有镜像行 " + mirrorRowPrefix(e.Target)
			}
		case "public_ip":
			r.Unverifiable = true
			r.Actual = "绑定的 Target 是控制面记录 id，节点规则按公网 IP 登记，无法逐项核对"
		default:
			r.Unverifiable = true
			r.Actual = "节点无法按 " + e.Kind + " 的 Target 定位规则，未核对"
		}
		results = append(results, r)
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.PlatformCheckKey: results}}, nil
}

// decodeExpectations 把参数里的期望清单解出来。
//
// 通道侧 params 是 JSON 还原的 map[string]any，这里二次转 JSON 再解到
// 结构体——比逐字段断言 map 直观，且两种历史形状（切片/JSON 文本）兼容。
func decodeExpectations(raw any, out *[]agent.PlatformExpectation) error {
	switch v := raw.(type) {
	case string:
		return jsonUnmarshalTo([]byte(v), out)
	default:
		b, err := jsonMarshal(v)
		if err != nil {
			return err
		}
		return jsonUnmarshalTo(b, out)
	}
}

// migrateAssess 实现 vm.migrate.assess（F-2-15 预检的量化依据）。
//
// P1 口径：**估算**。真实测速（节点间传输计时）与脏页采样（domdirtyrate）
// 需要两节点协同与对运行中机器的额外开销，随 P5 迁移功能一起接入——
// 控制面对 Bandwidth=0 的处理路径就是「回落到按链路规格估算」，语义完备。
func migrateAssess(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "assess", Name: "线路评估", Index: 1, Total: 2})
	info := agent.MigrateAssessInfo{
		Source:  agent.AssessSourceEstimate,
		Message: "按源节点默认路由网卡的协商速率估算；跨节点实测需双端协同",
	}
	// 源侧上行速率是一个可用的估算上限；读不到时 Bandwidth=0，控制面
	// 会回落到自己的链路规格估算。
	if iface := defaultRouteIface(ctx); iface != "" {
		if b, err := osReadFile("/sys/class/net/" + iface + "/speed"); err == nil {
			if mbps, err := strconv.ParseInt(strings.TrimSpace(b), 10, 64); err == nil && mbps > 0 {
				info.BandwidthMbps = mbps
				info.Message = "按源节点 " + iface + " 的协商速率 " + strconv.FormatInt(mbps, 10) + " Mbps 估算"
			}
		}
	}

	// 热迁移评估：触发一次脏页计算后再读 domstats，否则 dirtyrate
	// 永远是 0（Libvirt 只在计算过之后才给值）。
	live := false
	if v, ok := op.Params["live"].(bool); ok {
		live = v
	}
	if live {
		report(Stage{Key: "dirty_rate", Name: "采样脏页速率", Index: 2, Total: 2})
		_, _ = sys.Run(ctx, "virsh", "-c", "qemu:///system", "domdirtyrate-calc", op.Target, "1")
		time.Sleep(1100 * time.Millisecond)
		if s, err := sys.DomainStats(ctx, op.Target); err == nil && s.DirtyRateMBps > 0 {
			info.DirtyRateMBps = int64(s.DirtyRateMBps)
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.MigrateAssessDataKey: info}}, nil
}
