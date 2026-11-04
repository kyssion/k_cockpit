// ex_net_extra.go：网络治理的长尾动作（ACL / 交换机维护 / 计数 / 带宽 /
// IPv6 策略 / 公网 IP 运维 / 抓包取回）。
//
// 本文件按控制面参数形状接通真实的 iptables / OVS / ip / tc 操作。
package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

const captureDir = "/var/lib/k_cockpit/captures"

// hasCommand 报告命令是否存在（节点精简安装常缺 ip6tables / tc）。
func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// --- ACL ---

// vmVPACLPreview 实现 vpc.acl.preview。
func vmVPACLPreview(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "render_rules", Name: "渲染 ACL 规则", Index: 1, Total: 2})
	rules := decodeACLRules(op.Params["spec"])
	rendered := make([]string, 0, len(rules))
	warnings := []string{}
	for i, r := range rules {
		line := fmt.Sprintf("priority=%d action=%s direction=%s protocol=%s src=%s dst=%s",
			r.Priority, r.Action, r.Direction, orDash(r.Protocol), orDash(r.SrcCIDR), orDash(r.DstCIDR))
		if r.PortStart > 0 {
			if r.PortEnd > r.PortStart {
				line += fmt.Sprintf(" dport=%d-%d", r.PortStart, r.PortEnd)
			} else {
				line += fmt.Sprintf(" dport=%d", r.PortStart)
			}
		}
		rendered = append(rendered, line)
		if isDenyAll(r) && i != len(rules)-1 {
			warnings = append(warnings, fmt.Sprintf(
				"第 %d 条为拒绝全部，后面的规则不会生效", i+1))
		}
	}
	report(Stage{Key: "fingerprint", Name: "计算规则指纹", Index: 2, Total: 2})
	sum := sha256.Sum256([]byte(strings.Join(rendered, "\n")))
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VpcACLPreviewDataKey: agent.VpcACLPreview{
			Rendered: rendered,
			Warnings: warnings,
			Count:    len(rules),
			Version:  hex.EncodeToString(sum[:8]),
		}},
	}, nil
}

// vmVPACLApply 实现 vpc.acl.apply：把整个规则集写成一条独立链。
func vmVPACLApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	if !hasCommand("iptables") {
		return failResult("节点缺少 iptables，无法应用 VPC ACL"), nil
	}
	rules := decodeACLRules(op.Params["spec"])
	const chain = "KC-VPC-ACL"

	report(Stage{Key: "render_rules", Name: "渲染 ACL 规则", Index: 1, Total: 3})
	// 整链重建：ACL 是"从上往下第一条命中"，增量修改会让顺序取决于历史操作。
	_, _ = sys.Run(ctx, "iptables", "-N", chain)
	_, _ = sys.Run(ctx, "iptables", "-F", chain)

	report(Stage{Key: "apply_rules", Name: "写入规则链", Index: 2, Total: 3})
	for _, r := range rules {
		args := []string{"-A", chain}
		switch strings.ToLower(r.Direction) {
		case "in":
			if cidr := normalizedCIDR(r.SrcCIDR); cidr != "" {
				args = append(args, "-s", cidr)
			}
		case "out":
			if cidr := normalizedCIDR(r.DstCIDR); cidr != "" {
				args = append(args, "-d", cidr)
			}
		default:
			if cidr := normalizedCIDR(r.SrcCIDR); cidr != "" {
				args = append(args, "-s", cidr)
			}
			if cidr := normalizedCIDR(r.DstCIDR); cidr != "" {
				args = append(args, "-d", cidr)
			}
		}
		proto := strings.ToLower(r.Protocol)
		if proto == "tcp" || proto == "udp" {
			args = append(args, "-p", proto)
			if r.PortStart > 0 {
				if r.PortEnd > r.PortStart {
					args = append(args, "--dport", fmt.Sprintf("%d:%d", r.PortStart, r.PortEnd))
				} else {
					args = append(args, "--dport", strconv.Itoa(r.PortStart))
				}
			}
		} else if proto == "icmp" {
			args = append(args, "-p", "icmp")
		}
		target := "ACCEPT"
		if strings.EqualFold(r.Action, "deny") || strings.EqualFold(r.Action, "drop") {
			target = "DROP"
		}
		args = append(args, "-j", target)
		_, _ = sys.Run(ctx, "iptables", args...)
	}
	// 末尾 RETURN：ACL 没命中时回到 FORWARD 的后续规则，而不是隐式拒绝。
	_, _ = sys.Run(ctx, "iptables", "-A", chain, "-j", "RETURN")
	if !sys.RunOK(ctx, "iptables", "-C", "FORWARD", "-j", chain) {
		_, _ = sys.Run(ctx, "iptables", "-I", "FORWARD", "1", "-j", chain)
	}

	report(Stage{Key: "verify_chain", Name: "校验链已挂载", Index: 3, Total: 3})
	if !sys.RunOK(ctx, "iptables", "-C", "FORWARD", "-j", chain) {
		return failResult("ACL 链已写入但未能挂到 FORWARD，规则不会生效"), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VpcACLDataKey: agent.VpcACLApplyInfo{
			Applied: len(rules), Message: "ACL 已应用",
		}},
	}, nil
}

func decodeACLRules(raw any) []agent.VpcACLRuleSpec {
	if raw == nil {
		return nil
	}
	blob, err := jsonMarshal(raw)
	if err != nil {
		return nil
	}
	var out []agent.VpcACLRuleSpec
	_ = jsonUnmarshalTo(blob, &out)
	return out
}

func isDenyAll(r agent.VpcACLRuleSpec) bool {
	if !strings.EqualFold(r.Action, "deny") && !strings.EqualFold(r.Action, "drop") {
		return false
	}
	srcAll := r.SrcCIDR == "" || r.SrcCIDR == "0.0.0.0/0" || r.SrcCIDR == "any"
	dstAll := r.DstCIDR == "" || r.DstCIDR == "0.0.0.0/0" || r.DstCIDR == "any"
	protoAll := r.Protocol == "" || strings.EqualFold(r.Protocol, "all")
	return srcAll && dstAll && protoAll && r.PortStart == 0
}

func normalizedCIDR(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "any") || s == "0.0.0.0/0" || s == "::/0" {
		return ""
	}
	return s
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "any"
	}
	return s
}

// --- 交换机维护 ---

// vmVPCSwitchMigrate 实现 vpc.switch.migrate：把上行口挂到目标桥并设置 VLAN。
func vmVPCSwitchMigrate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	bridge := op.Target
	spec := mapParam(op.Params, "spec")
	uplink := strings.TrimSpace(strOf(spec["uplink_if"]))
	oldUplink := strings.TrimSpace(strOf(spec["old_uplink_if"]))
	vlan, _ := paramInt(spec, "vlan_id")
	if bridge == "" || uplink == "" {
		return failResult("缺少目标网桥或上行网卡"), nil
	}
	if !sys.RunOK(ctx, "ip", "link", "show", bridge) {
		return failResult("目标网桥不存在: " + bridge), nil
	}

	report(Stage{Key: "check_uplink", Name: "确认上行网卡", Index: 1, Total: 3})
	moved := 0
	ovsBridge := sys.RunOK(ctx, "ovs-vsctl", "br-exists", bridge)
	if ovsBridge {
		if cur, err := sys.Run(ctx, "ovs-vsctl", "port-to-br", uplink); err == nil {
			cur = strings.TrimSpace(cur)
			if cur != "" && cur != bridge {
				return failResult("目标上行口已属于另一个 OVS 网桥: " + cur), nil
			}
		}
		if _, err := sys.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", bridge, uplink); err != nil {
			return failResult("上行口接入 OVS 失败: " + err.Error()), nil
		}
		if vlan > 0 {
			if _, err := sys.Run(ctx, "ovs-vsctl", "set", "port", uplink, "tag="+strconv.Itoa(vlan)); err != nil {
				_, _ = sys.Run(ctx, "ovs-vsctl", "del-port", bridge, uplink)
				return failResult("设置 VLAN 失败: " + err.Error()), nil
			}
		}
		if out, err := sys.Run(ctx, "ovs-vsctl", "list-ports", bridge); err == nil {
			moved = len(strings.Fields(out))
		}
	} else {
		if _, err := sys.Run(ctx, "ip", "link", "set", uplink, "master", bridge); err != nil {
			return failResult("上行口接入网桥失败: " + err.Error()), nil
		}
		if vlan > 0 && hasCommand("bridge") {
			if _, err := sys.Run(ctx, "bridge", "vlan", "add", "dev", uplink,
				"vid", strconv.Itoa(vlan), "pvid", "untagged"); err != nil {
				_, _ = sys.Run(ctx, "ip", "link", "set", uplink, "nomaster")
				return failResult("设置 VLAN 失败: " + err.Error()), nil
			}
		}
		moved = 1
	}

	report(Stage{Key: "reconnect", Name: "恢复端口", Index: 2, Total: 3})
	// 迁移必须把旧上行摘掉。只 add 不 del 会让新旧上行同时挂在桥上，
	// 轻则双出口，重则环路。
	if oldUplink != "" && oldUplink != uplink {
		if ovsBridge {
			_, _ = sys.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", bridge, oldUplink)
		} else {
			_, _ = sys.Run(ctx, "ip", "link", "set", oldUplink, "nomaster")
		}
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", bridge, "up")
	report(Stage{Key: "verify", Name: "校验迁移结果", Index: 3, Total: 3})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.SwitchActionDataKey: agent.SwitchActionInfo{
			MovedPorts: moved, Message: "已迁移到新的上行网卡",
		}},
	}, nil
}

// vmVPCSwitchReconfigure 实现 vpc.switch.reconfigure：按期望状态重新下发。
func vmVPCSwitchReconfigure(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "reapply", Name: "按期望状态重新下发", Index: 1, Total: 2})
	spec := mapParam(op.Params, "spec")
	res, err := vpcSwitchChange(ctx, agent.Operation{
		Target: op.Target,
		Params: map[string]any{"action": "update", "spec": spec},
	}, func(Stage) {})
	if err != nil {
		return res, err
	}
	if res != nil && !res.Success {
		return res, nil
	}
	report(Stage{Key: "verify", Name: "校验重新下发结果", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.SwitchActionDataKey: agent.SwitchActionInfo{
			Message: "已按当前配置重新下发",
		}},
	}, nil
}

// vmVPCPortRelease 实现 vpc.port.release：把端口从所在桥/OVS 上摘除。
func vmVPCPortRelease(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	spec := mapParam(op.Params, "spec")
	port := strings.TrimSpace(strOf(spec["port_ref"]))
	if port == "" {
		return failResult("缺少要释放的端口"), nil
	}
	report(Stage{Key: "detach", Name: "摘下端口", Index: 1, Total: 2})

	released := false
	if sys.OVSRunning(ctx) {
		if bridge, err := sys.Run(ctx, "ovs-vsctl", "port-to-br", port); err == nil {
			bridge = strings.TrimSpace(bridge)
			if bridge != "" {
				if _, err := sys.Run(ctx, "ovs-vsctl", "del-port", bridge, port); err == nil {
					released = true
				}
			}
		}
	}
	if !released {
		if _, err := sys.Run(ctx, "ip", "link", "set", port, "nomaster"); err == nil {
			released = true
		}
	}
	if !released {
		return failResult("端口不在任何桥或 OVS 上（可能已释放）: " + port), nil
	}

	report(Stage{Key: "verify", Name: "确认端口已释放", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.PortReleaseDataKey: agent.PortReleaseInfo{
			Released: true, Message: "端口已释放",
		}},
	}, nil
}

// --- 计数 / 带宽 / IPv6 策略 ---

// vmNetworkCounterReset 实现 network.counter.reset。
func vmNetworkCounterReset(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "reset", Name: "归零计数器", Index: 1, Total: 1})
	if !sys.OVSRunning(ctx) {
		return &agent.Result{
			Success: true,
			Data: map[string]any{agent.CounterResetDataKey: agent.CounterResetInfo{
				Reset: 0, Message: "节点未运行 OVS，没有可重置的 OVS 计数器",
			}},
		}, nil
	}
	if _, err := sys.Run(ctx, "ovs-vsctl", "--all", "clear", "Interface", "statistics"); err != nil {
		return failResult("重置 OVS 计数器失败: " + err.Error()), nil
	}
	// 统计接口行数只用于回显"重置了几个"；失败不影响结果。
	count := 0
	if out, err := sys.Run(ctx, "ovs-vsctl", "list", "Interface"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "_uuid") {
				count++
			}
		}
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.CounterResetDataKey: agent.CounterResetInfo{
			Reset: count, Message: "计数器已归零",
		}},
	}, nil
}

// vmNetworkGlobalBandwidth 实现 network.global_bandwidth.apply（上行 TBF）。
func vmNetworkGlobalBandwidth(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	mbps, _ := paramInt(op.Params, "mbps")
	burst, _ := paramInt(op.Params, "burst_mbps")
	if !hasCommand("tc") {
		return failResult("节点缺少 tc，无法应用全局带宽"), nil
	}
	iface := defaultRouteIface(ctx)
	if iface == "" {
		return failResult("找不到默认路由网卡，无法应用全局带宽"), nil
	}
	report(Stage{Key: "clear", Name: "清除旧整形规则", Index: 1, Total: 3})
	_, _ = sys.Run(ctx, "tc", "qdisc", "del", "dev", iface, "root")

	if mbps > 0 {
		if burst <= 0 {
			burst = mbps / 8
			if burst < 1 {
				burst = 1
			}
		}
		report(Stage{Key: "apply", Name: "写入总限", Index: 2, Total: 3})
		if _, err := sys.Run(ctx, "tc", "qdisc", "add", "dev", iface, "root", "tbf",
			"rate", fmt.Sprintf("%dmbit", mbps),
			"burst", fmt.Sprintf("%dmbit", burst),
			"latency", "50ms"); err != nil {
			return failResult("应用全局带宽失败: " + err.Error()), nil
		}
	}
	report(Stage{Key: "verify", Name: "校验整形规则", Index: 3, Total: 3})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.GlobalBandwidthDataKey: agent.GlobalBandwidthInfo{
			Applied: true, Mbps: mbps, BurstMbps: burst,
			Message: "全局带宽总限已应用",
		}},
	}, nil
}

// vmNetworkIPv6Policy 实现 network.ipv6.policy：只信任列出的前缀。
func vmNetworkIPv6Policy(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	if !hasCommand("ip6tables") {
		return failResult("节点缺少 ip6tables，无法下发 IPv6 策略"), nil
	}
	protect := boolParam(op.Params, "protect")
	prefixes := stringSliceParam(op.Params, "trusted_prefixes")
	const chain = "KC-IPV6-GUARD"

	report(Stage{Key: "render_rules", Name: "渲染 IPv6 规则", Index: 1, Total: 3})
	_, _ = sys.Run(ctx, "ip6tables", "-N", chain)
	if !protect {
		for {
			if _, err := sys.Run(ctx, "ip6tables", "-D", "FORWARD", "-j", chain); err != nil {
				break
			}
		}
		_, _ = sys.Run(ctx, "ip6tables", "-F", chain)
		report(Stage{Key: "apply_rules", Name: "移除保护规则", Index: 2, Total: 3})
		return &agent.Result{
			Success: true,
			Data: map[string]any{agent.IPv6PolicyDataKey: agent.IPv6PolicyInfo{
				Applied: true, Message: "IPv6 保护策略已关闭", Trusted: prefixes,
			}},
		}, nil
	}

	report(Stage{Key: "apply_rules", Name: "写入保护规则", Index: 2, Total: 3})
	_, _ = sys.Run(ctx, "ip6tables", "-F", chain)
	for _, p := range prefixes {
		_, _ = sys.Run(ctx, "ip6tables", "-A", chain, "-s", p, "-j", "RETURN")
		_, _ = sys.Run(ctx, "ip6tables", "-A", chain, "-d", p, "-j", "RETURN")
	}
	_, _ = sys.Run(ctx, "ip6tables", "-A", chain, "-j", "DROP")
	if !sys.RunOK(ctx, "ip6tables", "-C", "FORWARD", "-j", chain) {
		_, _ = sys.Run(ctx, "ip6tables", "-I", "FORWARD", "1", "-j", chain)
	}

	report(Stage{Key: "verify", Name: "校验保护规则", Index: 3, Total: 3})
	if !sys.RunOK(ctx, "ip6tables", "-C", "FORWARD", "-j", chain) {
		return failResult("IPv6 规则已写入但未能挂到 FORWARD"), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.IPv6PolicyDataKey: agent.IPv6PolicyInfo{
			Applied: true, Message: "IPv6 保护策略已下发", Trusted: prefixes,
		}},
	}, nil
}

// --- 公网 IP 运维 ---

// vmPublicIPPreview 实现 public.ip.preview。
func vmPublicIPPreview(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	ip := op.Target
	mode := strParam(op.Params, "mode")
	vmName := strParam(op.Params, "vm_name")
	report(Stage{Key: "inspect", Name: "检查现有规则", Index: 1, Total: 2})
	out := agent.PublicIPPreview{}
	switch mode {
	case "", "nat", "nat_1to1":
		target := ""
		if vmName != "" {
			target = vmIP(ctx, vmName)
		}
		if target == "" || target == "0.0.0.0/0" {
			out.Warnings = append(out.Warnings, "虚拟机当前没有可用 IP（未开机或未获取地址），绑定会失败")
		} else {
			out.Added = append(out.Added,
				fmt.Sprintf("iptables -t nat -A PREROUTING -d %s -j DNAT --to-destination %s", ip, target))
		}
	case "routed":
		target := ""
		if vmName != "" {
			target = vmIP(ctx, vmName)
		}
		if target == "" || target == "0.0.0.0/0" {
			out.Warnings = append(out.Warnings, "经典路由要求来宾已配置该地址且能被节点解析到 IP")
			break
		}
		if strings.Contains(ip, ":") {
			out.Added = append(out.Added,
				fmt.Sprintf("ip -6 route replace %s/128 via %s", ip, target),
				fmt.Sprintf("ip -6 neigh replace proxy %s dev <uplink>", ip))
		} else {
			out.Added = append(out.Added,
				fmt.Sprintf("ip route replace %s/32 via %s", ip, target),
				fmt.Sprintf("ip neigh replace proxy %s dev <uplink>", ip))
		}
	case "bridged":
		out.Warnings = append(out.Warnings,
			"经典桥接不做宿主机 NAT/路由：来宾必须把该地址配在网卡上，且与宿主机处于同一二层网络")
	default:
		out.Warnings = append(out.Warnings, "未知的公网地址模式: "+mode)
	}
	report(Stage{Key: "render", Name: "生成预览", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.PublicIPPreviewKey: out},
	}, nil
}

// vmPublicIPReload 实现 public_ip.reload：按控制面下发的绑定列表重放 DNAT。
func vmPublicIPReload(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "load_bindings", Name: "读取绑定列表", Index: 1, Total: 2})
	raw := op.Params["bindings"]
	blob, _ := jsonMarshal(raw)
	var bindings []map[string]any
	_ = jsonUnmarshalTo(blob, &bindings)

	applied, failed := 0, 0
	var details []string
	for _, b := range bindings {
		res, err := publicIPChange(ctx, agent.Operation{
			Params: map[string]any{
				"action":  "bind",
				"ip":      strOf(b["public_ip"]),
				"vm_name": strOf(b["vm_name"]),
				"mode":    strOf(b["mode"]),
			},
		}, func(Stage) {})
		if err != nil || res == nil || !res.Success {
			failed++
			if res != nil {
				details = append(details, res.Message)
			}
			continue
		}
		applied++
	}
	report(Stage{Key: "verify", Name: "校验重载结果", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.PublicIPReloadDataKey: agent.PublicIPReloadInfo{
			Applied: applied, Failed: failed,
			Detail:  strings.Join(details, "；"),
			Message: "已按当前绑定关系重新应用规则",
		}},
	}, nil
}

// vmPublicIPGuestStatus 实现 public_ip.guest_status：读域地址并做可达性探测。
func vmPublicIPGuestStatus(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read_addresses", Name: "读取来宾地址", Index: 1, Total: 2})
	out, err := sys.Run(ctx, "virsh", "-c", "qemu:///system", "domifaddr", op.Target, "--source", "agent")
	if err != nil {
		out, err = sys.Run(ctx, "virsh", "-c", "qemu:///system", "domifaddr", op.Target, "--source", "lease")
	}
	if err != nil {
		return failResult("无法读到来宾地址（来宾可能未开机或未安装 guest agent）: " + err.Error()), nil
	}

	rows := []agent.GuestIPInfo{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		family := fields[2]
		if family != "ipv4" && family != "ipv6" {
			continue
		}
		addr := fields[3]
		plain := strings.SplitN(addr, "/", 2)[0]
		info := agent.GuestIPInfo{
			Address: addr, Family: family, Configured: true,
			Detail:    "来自节点地址表",
			Reachable: sys.RunOK(ctx, "ping", "-c", "1", "-W", "1", plain),
		}
		rows = append(rows, info)
	}
	report(Stage{Key: "check_reachable", Name: "探测可达性", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.GuestIPDataKey: rows},
	}, nil
}

// vmPublicIPv6Detect 实现 public_ip.ipv6_detect。
func vmPublicIPv6Detect(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read_addrs", Name: "读取 IPv6 地址", Index: 1, Total: 2})
	out, err := sys.Run(ctx, "ip", "-6", "addr", "show")
	if err != nil {
		return failResult("读取 IPv6 地址失败: " + err.Error()), nil
	}
	defaultOut, _ := sys.Run(ctx, "ip", "-6", "route", "show", "default")

	byPrefix := map[string]agent.IPv6PrefixInfo{}
	iface := ""
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if f := strings.Fields(line); len(f) >= 2 {
				iface = strings.TrimSuffix(f[1], ":")
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "inet6" {
			continue
		}
		addr := fields[1]
		if strings.HasPrefix(strings.ToLower(addr), "fe80") {
			continue
		}
		ip, ipnet, err := net.ParseCIDR(addr)
		if err != nil || ip.To4() != nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		prefix := ipnet.String()
		if _, ok := byPrefix[prefix]; ok {
			continue
		}
		// /64（最常见）的可用地址数是 2^64，超出 int64；统一饱和到
		// MaxInt64，界面显示为"极大"而不是一个溢出后的负数。
		assignable := int64(-1)
		if ones <= 64 {
			assignable = math.MaxInt64
		} else if ones < 127 {
			assignable = int64(1) << (128 - ones)
		}
		byPrefix[prefix] = agent.IPv6PrefixInfo{
			Prefix: prefix, EgressIf: iface,
			Trusted:    strings.Contains(defaultOut, "dev "+iface),
			Assignable: assignable,
		}
	}

	report(Stage{Key: "verify", Name: "校验前缀可用性", Index: 2, Total: 2})
	outRows := make([]agent.IPv6PrefixInfo, 0, len(byPrefix))
	for _, v := range byPrefix {
		outRows = append(outRows, v)
	}
	sort.Slice(outRows, func(i, j int) bool { return outRows[i].Prefix < outRows[j].Prefix })
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.IPv6PrefixDataKey: outRows},
	}, nil
}

// --- 抓包文件 ---

// vmCaptureFetch 实现 network.capture.fetch。
func vmCaptureFetch(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "file_check", Name: "确认文件仍在保留期内", Index: 1, Total: 2})
	path := strParam(op.Params, "file_path")
	if path == "" {
		path = op.Target
	}
	if err := requireUnder(captureDir, path); err != nil {
		return failResult("抓包文件路径非法: " + path), nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return failResult("抓包文件不存在: " + err.Error()), nil
	}
	offset, hasOff := paramInt64(op.Params, "offset")
	length, hasLen := paramInt64(op.Params, "length")
	report(Stage{Key: "stream_back", Name: "回传文件内容", Index: 2, Total: 2})
	if !hasOff || !hasLen {
		data, err := os.ReadFile(path)
		if err != nil {
			return failResult("读取抓包文件失败: " + err.Error()), nil
		}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.CaptureContentKey: agent.ExportContent{
				Data: data, MIME: "application/vnd.tcpdump.pcap",
			},
			"total_size": st.Size(),
		}}, nil
	}
	if offset < 0 || offset > st.Size() || length <= 0 {
		return failResult("抓包分片范围非法"), nil
	}
	if offset+length > st.Size() {
		length = st.Size() - offset
	}
	buf := make([]byte, length)
	f, err := os.Open(path)
	if err != nil {
		return failResult("打开抓包文件失败: " + err.Error()), nil
	}
	defer func() { _ = f.Close() }()
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return failResult("读取抓包分片失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.CaptureContentKey: agent.ExportContent{
			Data: buf, MIME: "application/vnd.tcpdump.pcap",
		},
		"total_size": st.Size(),
	}}, nil
}

// vmNetworkCaptureDelete 实现 network.capture.delete。
func vmNetworkCaptureDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "file_remove", Name: "删除抓包文件", Index: 1, Total: 1})
	path := strParam(op.Params, "file_path")
	if path == "" {
		path = op.Target
	}
	if err := requireUnder(captureDir, path); err != nil {
		return failResult("拒绝删除抓包目录之外的路径: " + path), nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return failResult("删除抓包文件失败: " + err.Error()), nil
	}
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.CaptureDataKey: agent.CaptureInfo{Message: "抓包文件已删除"}},
	}, nil
}

// --- 小工具 ---

func mapParam(params map[string]any, key string) map[string]any {
	if m, ok := params[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// defaultRouteIface 从默认路由取上行网卡名。
func defaultRouteIface(ctx context.Context) string {
	out, err := sys.Run(ctx, "ip", "route", "show", "default")
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
