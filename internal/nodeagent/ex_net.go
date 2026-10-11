// ex_net.go：网络探测执行器（network.probe / ovs.* / dhcp.leases /
// platform.check / vm.migrate.assess）。
package nodeagent

import (
	"context"
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
// P1 的实付校验能力以「OVS 端口存在性」为主；端口安全流表、公网 NAT 规则
// 的深度校验随 P3 网络能力接入。此时该操作在控制面侧通常没有期望项可查
// （那些功能尚未开通），实现保持薄而正确。
func platformCheck(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "check", Name: "逐项核对", Index: 1, Total: 1})
	var expectations []agent.PlatformExpectation
	if raw, ok := op.Params["expectations"]; ok {
		_ = decodeExpectations(raw, &expectations)
	}
	results := make([]agent.PlatformCheckResult, 0, len(expectations))
	for _, e := range expectations {
		r := agent.PlatformCheckResult{ID: e.ID, Present: false, Actual: "该类别的深度校验随网络能力（P3）接入"}
		switch e.Kind {
		case "port_security":
			// Target 是网口引用：能确认 OVS 上是否有这个口。
			if _, err := sys.OVSInterfaceBridge(ctx, e.Target); err == nil {
				r.Present = true
				r.Actual = "端口存在于 OVS（流表深度校验随 P3 接入）"
			} else {
				r.Actual = "OVS 上没有该端口"
			}
		default:
			r.Actual = "该类别的校验随 " + e.Kind + " 能力分期接入"
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
	report(Stage{Key: "assess", Name: "线路评估", Index: 1, Total: 1})
	info := agent.MigrateAssessInfo{
		Source:  "estimate",
		Message: "链路实测与脏页采样随迁移功能（P5）接入，当前为估算口径",
	}
	// 运行中的机器顺带读一次脏页（domstats 的 dirtyrate 键只在触发过计算
	// 后有值，读不到就是 0，控制面按「未测得」处理）。
	live := false
	if v, ok := op.Params["live"].(bool); ok {
		live = v
	}
	if live {
		if s, err := sys.DomainStats(ctx, op.Target); err == nil && s.DirtyRateMBps > 0 {
			info.DirtyRateMBps = int64(s.DirtyRateMBps)
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.MigrateAssessDataKey: info}}, nil
}
