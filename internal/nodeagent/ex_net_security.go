// ex_net_security.go：网络安全类写执行器（P3 第二批）——安全组（per-VM
// iptables 链）、节点防火墙（基线 + 紧急回滚）、端口转发（DNAT）、
// 静态地址（dnsmasq host 条目）、公网地址（DNAT 池规则）、端口安全
// （OVS 流表）、端口镜像（OVS mirror）、抓包（tcpdump）。
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// timeNowSec / fileSizeOr0 / osMkdirAll / timeTimer / schedule
// 是本文件用到的小助手。
func timeNowSec() int64 { return time.Now().Unix() }

func fileSizeOr0(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

func osMkdirAll(path string) { _ = os.MkdirAll(path, 0o755) }

func schedule(sec int, f func()) *time.Timer {
	return time.AfterFunc(time.Duration(sec)*time.Second, f)
}

// iptables 幂等三件套：检查存在（-C）→ 不存在才追加（-A）；删除用 -D。
func iptablesHave(ctx context.Context, args ...string) bool {
	full := append([]string{"-C"}, args...)
	return sys.RunOK(ctx, "iptables", full...)
}

func iptablesAdd(ctx context.Context, args ...string) {
	if !iptablesHave(ctx, args...) {
		_, _ = sys.Run(ctx, "iptables", append([]string{"-A"}, args...)...)
	}
}

func iptablesDel(ctx context.Context, args ...string) {
	// -D 对不存在的规则会报错，静默。
	_, _ = sys.Run(ctx, "iptables", append([]string{"-D"}, args...)...)
}

// iptablesNatAdd/Del 是 nat 表版本的幂等三件套。
//
// 不能走 iptablesAdd：它把 -C 前置到整个参数串，而 `-C` 把紧跟的参数当
// 链名——`iptables -C -t nat ...` 会把 "-t" 当链名解析成垃圾（真机排查
// 端口转发时的同族发现）。nat 表规则必须 `-t nat` 打头、链名显式给出。
func iptablesNatAdd(ctx context.Context, chain string, spec ...string) {
	args := append([]string{"-t", "nat", "-C", chain}, spec...)
	if !sys.RunOK(ctx, "iptables", args...) {
		_, _ = sys.Run(ctx, "iptables", append([]string{"-t", "nat", "-A", chain}, spec...)...)
	}
}

func iptablesNatDel(ctx context.Context, chain string, spec ...string) {
	_, _ = sys.Run(ctx, "iptables", append([]string{"-t", "nat", "-D", chain}, spec...)...)
}

// securityGroupApply 实现 security_group.apply：per-VM 生效规则整台下发。
//
// 实现：专属链 KC-SG-<vm> + 按 VM 当前 IP 的入/出规则，最后把链挂进
// FORWARD（-d/-s 各一条跳板）。链建好不挂 FORWARD 等于没建——review
// 抓出的空转缺陷：规则全部就位但没有任何流量经过链。OVS 原生流表口径
// 随端口安全一并深化。
func securityGroupApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "render", Name: "渲染规则链", Index: 1, Total: 2})
	vm := op.Target
	chain := "KC-SG-" + strings.ReplaceAll(vm, "-", "_")
	_, _ = sys.Run(ctx, "iptables", "-N", chain) // 已存在则报错，忽略

	// 重建：先清空。
	_, _ = sys.Run(ctx, "iptables", "-F", chain)

	ip := vmIP(ctx, vm)
	// rules: [{direction, protocol, port_start, port_end, target_type, target_value}]
	rulesAny, _ := op.Params["rules"].([]any)
	applied := 0
	for _, r := range rulesAny {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		direction, _ := m["direction"].(string)
		protocol, _ := m["protocol"].(string)
		if protocol == "" || protocol == "any" {
			protocol = "all"
		}
		var port string
		if ps, ok := m["port_start"].(float64); ok {
			port = strconv.Itoa(int(ps))
			if pe, ok := m["port_end"].(float64); ok && int(pe) != int(ps) {
				port += ":" + strconv.Itoa(int(pe))
			}
		}
		targetType, _ := m["target_type"].(string)
		targetValue, _ := m["target_value"].(string)
		// 入站规则：-s 来源 → 允许进入该 VM IP；出站：-d 目标。
		var base []string
		switch direction {
		case "in":
			base = []string{"-d", ip}
			if targetType == "cidr" || targetType == "ip" {
				base = append(base, "-s", targetValue)
			}
		case "out":
			base = []string{"-s", ip}
			if targetType == "cidr" || targetType == "ip" {
				base = append(base, "-d", targetValue)
			}
		default:
			continue
		}
		args := []string{"-p", strings.ToLower(protocol)}
		args = append(args, base...)
		if port != "" && protocol != "all" {
			args = append(args, "--dport", port)
		}
		args = append(args, "-j", "ACCEPT")
		iptablesAdd(ctx, append([]string{chain}, args...)...)
		applied++
	}
	// 默认拒绝：链尾 DROP（并集模型下由控制面保证只发允许规则）。
	iptablesAdd(ctx, chain, "-j", "DROP")

	report(Stage{Key: "apply", Name: "下发规则", Index: 2, Total: 2})
	msg := fmt.Sprintf("已下发 %d 条生效规则（链 %s）", applied, chain)
	if ip == "0.0.0.0/0" {
		// 解析不到 VM IP 时**不挂 FORWARD**：拿兜底网段做跳板会把全宿主机
		// 的转发流量都引进这条带 DROP 链尾的链——等于掐断所有虚拟机网络。
		// 规则照建（IP 出现后再 apply 一次即生效），如实说明未接入。
		msg += "；未解析到虚拟机 IP，规则尚未接入转发路径（开机后重新应用）"
	} else {
		// 挂载幂等：先删后加。VM 换 IP 后旧跳板残留无害（旧 IP 已无人使用，
		// 新链规则按新 IP 匹配），清理依赖下次 apply 时链被 -F 重建。
		for _, dir := range [][]string{{"-d", ip}, {"-s", ip}} {
			iptablesDel(ctx, append([]string{"FORWARD"}, append(dir, "-j", chain)...)...)
			iptablesAdd(ctx, append([]string{"FORWARD"}, append(dir, "-j", chain)...)...)
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.SecurityGroupDataKey: agent.SecurityGroupInfo{
			Applied: applied, Message: msg,
		},
	}}, nil
}

// vmIP 尽力拿到虚拟机当前 IP（domifaddr → 该机自己的 vnet 口上的邻居）。
//
// 邻居兜底必须限定**这台虚拟机自己的网口**：此前返回第一个 vnet 邻居的
// IP，多虚拟机宿主机上会串号（把别的机器的 IP 当成这台的——安全组与
// 公网 DNAT 都会绑错对象）。
func vmIP(ctx context.Context, vm string) string {
	if out, err := sys.Run(ctx, "virsh", "-r", "-c", "qemu:///system", "domifaddr", vm); err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 4 && strings.Contains(fields[3], "/") {
				return strings.SplitN(fields[3], "/", 2)[0]
			}
		}
	}
	xml, err := sys.DumpXML(ctx, vm, true)
	if err != nil {
		return "0.0.0.0/0"
	}
	vnets := parseVnetTargets(xml)
	if len(vnets) == 0 {
		return "0.0.0.0/0"
	}
	entries, _ := sys.ListNeighbors(ctx)
	for _, e := range entries {
		if vnets[e.Interface] {
			return e.IP
		}
	}
	return "0.0.0.0/0"
}

// firewallApply 实现 firewall.apply（节点级基线：默认策略 + 白名单 +
// 规则集）。**先写入后收紧**：先建链放行白名单与既有连接，再收紧 INPUT。
func firewallApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	enabled := boolParam(op.Params, "enabled")
	defaultAction := strParam(op.Params, "default_action")
	report(Stage{Key: "baseline", Name: "写入基线链", Index: 1, Total: 2})

	const chain = "KC-FW"
	_, _ = sys.Run(ctx, "iptables", "-N", chain)
	_, _ = sys.Run(ctx, "iptables", "-F", chain)

	// 既有连接与回环先放行（收紧的前置保护）。
	iptablesAdd(ctx, chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	iptablesAdd(ctx, chain, "-i", "lo", "-j", "ACCEPT")
	// 白名单。
	if wl, ok := op.Params["whitelist"].([]any); ok {
		for _, w := range wl {
			if s, ok := w.(string); ok && s != "" {
				iptablesAdd(ctx, chain, "-s", s, "-j", "ACCEPT")
			}
		}
	}
	// 规则集。控制面的动作词表是 accept/deny（model.FirewallAccept/Deny），
	// 键名是 port_start/port_end/source_cidr（RuleView 的 json tag）——此前
	// agent 按 allow/m["port"]/m["source"] 读取，三处全 miss：accept 规则
	// 被判成 DROP（真机实测「放行 443」落成 -p tcp -j DROP）、端口与来源
	// 全部丢失。按真实下发形状对齐。
	if rules, ok := op.Params["rules"].([]any); ok {
		for _, r := range rules {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			action, _ := m["action"].(string)
			target := "DROP"
			if action == "allow" || action == "accept" {
				target = "ACCEPT"
			}
			proto, _ := m["protocol"].(string)
			args := []string{"-p", proto}
			if proto == "" {
				args = []string{}
			}
			ps, psOK := m["port_start"].(float64)
			if psOK && ps > 0 && proto != "" {
				port := strconv.Itoa(int(ps))
				// 没有 -p 的 --dport 是非法规则（iptables 报错被 iptablesAdd
				// 吞掉，规则静默缺失）——无协议时端口无从匹配，忽略。
				if pe, ok := m["port_end"].(float64); ok && pe >= ps {
					port += ":" + strconv.Itoa(int(pe))
				}
				args = append(args, "--dport", port)
			}
			if src, ok := m["source_cidr"].(string); ok && src != "" {
				args = append(args, "-s", src)
			}
			// 没有 -j 的规则只是计数器：匹配后什么都不做，allow/deny
			// 全部静默无效（review 抓出——此前 target 算完被丢弃）。
			args = append(args, "-j", target)
			iptablesAdd(ctx, append([]string{chain}, args...)...)
		}
	}

	report(Stage{Key: "tighten", Name: "接入 INPUT", Index: 2, Total: 2})
	// 接入点：INPUT 1 插入跳板（enabled=false 时摘除）。
	_, _ = sys.Run(ctx, "iptables", "-D", "INPUT", "-j", chain)
	if enabled {
		if _, err := sys.Run(ctx, "iptables", "-I", "INPUT", "1", "-j", chain); err != nil {
			return failResult("接入 INPUT 失败: " + err.Error()), nil
		}
		// 默认策略是整套防火墙最关键的一条——失败必须如实报，否则
		// 「收紧失败 + 报告成功」等于裸奔（review 抓出）。控制面的词表是
		// deny（model.FirewallDeny），不是 drop——只认 drop 会让 deny
		// 策略静默不收紧（同族键位错位，真机防火墙测试连带抓出）。
		policy := "ACCEPT"
		if defaultAction == "drop" || defaultAction == "deny" {
			policy = "DROP" // 收紧默认策略放最后（先写入后收紧）。
		}
		if perr := sys.Run0(ctx, "iptables", "-P", "INPUT", policy); perr != nil {
			return failResult("设置 INPUT 默认策略失败: " + perr.Error()), nil
		}
	} else {
		_, _ = sys.Run(ctx, "iptables", "-P", "INPUT", "ACCEPT")
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.FirewallDataKey: agent.FirewallInfo{Applied: 1, Message: "防火墙基线已应用"},
	}}, nil
}

// firewallRollback 实现 firewall.rollback：紧急回滚不设门槛也不应失败。
func firewallRollback(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "rollback", Name: "紧急回滚", Index: 1, Total: 1})
	// 两层防火墙（节点 KC-FW / 宿主机 KC-HFW）同挂 INPUT：紧急回滚的
	// 语义是「恢复全通」，只清一层会让另一层继续过滤（真机实证：宿主机
	// 回滚后 KC-HFW 六条规则与 INPUT 跳板全部残留）。
	for _, chain := range []string{"KC-FW", "KC-HFW"} {
		_, _ = sys.Run(ctx, "iptables", "-D", "INPUT", "-j", chain)
		_, _ = sys.Run(ctx, "iptables", "-F", chain)
		_, _ = sys.Run(ctx, "iptables", "-X", chain)
	}
	_, _ = sys.Run(ctx, "iptables", "-P", "INPUT", "ACCEPT")
	return &agent.Result{Success: true, Data: map[string]any{
		agent.FirewallDataKey: agent.FirewallInfo{Applied: 0, Message: "防火墙已回滚为全通"},
	}}, nil
}

// hostFirewallApply 实现 host_firewall.apply（第一层：SSH/面板保护 + 规则）。
//
// 与 firewall.apply 同族，但控制面的下发形状不同（真机防火墙测试连带
// 抓出的键位错位）：whitelist 是**逗号分隔字符串**而非列表，规则键是
// port_start/port_end/source_cidr、动作词表 accept/deny——此前 rules
// 完全没被读取（自定义规则静默丢弃）、whitelist 按 []any 断言恒失败。
func hostFirewallApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	if action == "rollback" {
		return firewallRollback(ctx, op, report)
	}
	enabled := boolParam(op.Params, "enabled")
	defaultAction := strParam(op.Params, "default_action")
	report(Stage{Key: "apply", Name: "应用宿主机防火墙", Index: 1, Total: 1})

	const chain = "KC-HFW"
	_, _ = sys.Run(ctx, "iptables", "-N", chain)
	_, _ = sys.Run(ctx, "iptables", "-F", chain)
	iptablesAdd(ctx, chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
	iptablesAdd(ctx, chain, "-i", "lo", "-j", "ACCEPT")
	iptablesAdd(ctx, chain, "-p", "tcp", "--dport", "22", "-j", "ACCEPT") // SSH 兜底保护
	// 白名单：控制面发的是逗号分隔字符串；列表形态也容忍（与 firewall.apply 共用形状）。
	if wl, ok := op.Params["whitelist"].(string); ok {
		for _, w := range strings.Split(wl, ",") {
			if w = strings.TrimSpace(w); w != "" {
				iptablesAdd(ctx, chain, "-s", w, "-j", "ACCEPT")
			}
		}
	} else if wl, ok := op.Params["whitelist"].([]any); ok {
		for _, w := range wl {
			if s, ok := w.(string); ok && s != "" {
				iptablesAdd(ctx, chain, "-s", s, "-j", "ACCEPT")
			}
		}
	}
	// 规则集（含控制面合成的面板端口等保护规则——丢了它们应用后立刻失联）。
	applied := 0
	if rules, ok := op.Params["rules"].([]any); ok {
		for _, r := range rules {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			a, _ := m["action"].(string)
			target := "DROP"
			if a == "allow" || a == "accept" {
				target = "ACCEPT"
			}
			proto, _ := m["protocol"].(string)
			args := []string{"-p", proto}
			if proto == "" {
				args = []string{}
			}
			if ps, ok := m["port_start"].(float64); ok && ps > 0 && proto != "" {
				port := strconv.Itoa(int(ps))
				if pe, ok := m["port_end"].(float64); ok && pe >= ps {
					port += ":" + strconv.Itoa(int(pe))
				}
				args = append(args, "--dport", port)
			}
			if src, ok := m["source_cidr"].(string); ok && src != "" {
				args = append(args, "-s", src)
			}
			args = append(args, "-j", target)
			iptablesAdd(ctx, append([]string{chain}, args...)...)
			applied++
		}
	}
	// 兜底拒绝放在所有放行之后：默认动作必须由**本链**兜住，
	// 而不能依赖宿主机恰好已有 DROP 策略——那会让"默认拒绝"在
	// 没有 ufw 的主机上形同虚设（实机复现：INPUT 仍是 ACCEPT）。
	//
	// 顺序是语义的一部分：这条必须排在白名单与规则集的 -j ACCEPT 之后，
	// 否则任何放行都不可能命中。链每次 apply 都被 -F 重建，因此重复 apply
	// 不会叠加（幂等）。
	if enabled && (defaultAction == "deny" || defaultAction == "drop") {
		iptablesAdd(ctx, chain, "-j", "DROP")
	}
	_, _ = sys.Run(ctx, "iptables", "-D", "INPUT", "-j", chain)
	if enabled {
		if _, err := sys.Run(ctx, "iptables", "-I", "INPUT", "1", "-j", chain); err != nil {
			return failResult(err.Error()), nil
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.HostFirewallDataKey: agent.HostFirewallInfo{
			AppliedRules: applied, Message: "宿主机防火墙已应用",
		},
	}}, nil
}

// vmPortForwardChange 实现 vm.portforward.change（DNAT 增删）。
//
// 规则形状（映射管理页语义）：action=add/remove；protocol tcp/udp；
// host_port → target_ip:target_port；allowed_ips 可选来源限制。
func vmPortForwardChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "nat", Name: "调整 DNAT 规则", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	protocol := strParam(op.Params, "protocol")
	if protocol == "" {
		protocol = "tcp"
	}
	hostPort, _ := op.Params["host_port"].(float64)
	targetPort, _ := op.Params["target_port"].(float64)
	targetIP := strParam(op.Params, "target_ip")

	// DNAT 在 PREROUTING 上做；此前既缺链名又把 -t nat 放在 iptablesAdd
	// 的 -C 之后（-C 会把 "-t" 当链名），规则永远加不上（真机同族排查）。
	dnat := []string{"-p", protocol, "--dport", strconv.Itoa(int(hostPort))}
	fw := []string{"-p", protocol, "--dport", strconv.Itoa(int(hostPort))}
	if src, ok := op.Params["allowed_ips"].(string); ok && src != "" {
		dnat = append(dnat, "-s", src)
		fw = append(fw, "-s", src)
	}
	dnat = append(dnat, "-j", "DNAT", "--to-destination", targetIP+":"+strconv.Itoa(int(targetPort)))

	switch action {
	case "add":
		iptablesNatAdd(ctx, "PREROUTING", dnat...)
		// 转发路径放行（filter 表，FORWARD 链——iptablesAdd 的形态对
		// filter 表成立：-C 后紧跟链名）。
		iptablesAdd(ctx, append([]string{"FORWARD", "-d", targetIP}, fw...)...)
		iptablesAdd(ctx, "FORWARD", "-d", targetIP, "-j", "ACCEPT")
	case "remove":
		// add/remove 必须对称：remove 只摘 DNAT 会把 add 写入的两条
		// FORWARD 放行留在宿主机上（真机实证的残留）。
		iptablesNatDel(ctx, "PREROUTING", dnat...)
		iptablesDel(ctx, append([]string{"FORWARD", "-d", targetIP}, fw...)...)
		iptablesDel(ctx, "FORWARD", "-d", targetIP, "-j", "ACCEPT")
	default:
		return failResult("未知转发动作: " + action), nil
	}
	return &agent.Result{Success: true}, nil
}

// vmStaticIPChange 实现 vm.staticip.change（dnsmasq host 条目）。
func vmStaticIPChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "static", Name: "登记静态地址", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	// 手工配置的地址只存在于来宾内部，控制面记录即可，节点无需动作。
	if !boolParam(op.Params, "is_dhcp_reservation") {
		return &agent.Result{Success: true, Message: "手工配置的静态地址无需节点侧 DHCP 动作"}, nil
	}
	mac := strings.TrimSpace(strParam(op.Params, "mac"))
	bridge := strings.TrimSpace(strParam(op.Params, "bridge_name"))
	ip := strings.TrimSpace(strParam(op.Params, "ip"))
	start := strings.TrimSpace(strParam(op.Params, "dhcp_start"))
	end := strings.TrimSpace(strParam(op.Params, "dhcp_end"))
	switch action {
	case "bind":
		if mac == "" || bridge == "" || ip == "" {
			return failResult("DHCP 静态租约缺少 MAC、网桥或地址"), nil
		}
		if strings.Contains(ip, ":") {
			return failResult("DHCP 静态租约当前只支持 IPv4；IPv6 地址应走手工配置"), nil
		}
		if err := upsertDHCPHost(ctx, bridge, mac, ip, start, end); err != nil {
			return failResult("写入 DHCP 静态租约失败: " + err.Error()), nil
		}
		return &agent.Result{Success: true, Message: "DHCP 静态租约已生效"}, nil
	case "unbind":
		if mac == "" || bridge == "" {
			return &agent.Result{Success: true, Message: "静态地址已解绑（无需节点侧动作）"}, nil
		}
		if err := removeDHCPHost(ctx, bridge, mac); err != nil {
			return failResult("删除 DHCP 静态租约失败: " + err.Error()), nil
		}
		return &agent.Result{Success: true, Message: "DHCP 静态租约已删除"}, nil
	default:
		return failResult("未知静态地址动作: " + action), nil
	}
}

// publicIPChange 实现 public.ip.change（bind/unbind/migrate 的 DNAT）。
//
// 目标 VM 的 IP 由节点解析：控制面的 executor 只发 vm_name、从不发
// vm_ip（真机 bind 报成功但 DNAT 0 条——空 --to-destination 的 iptables
// 报错被吞）。bind/migrate 解析不到 IP 时如实失败；unbind 时 VM 可能已
// 关机解析不到，按「-d <ip> + DNAT」的形状从规则集匹配删除，不要求重现
// 完整的 --to-destination。
func publicIPChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "public_ip", Name: "调整公网地址", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	ip := strParam(op.Params, "ip")
	vmName := strParam(op.Params, "vm_name")
	mode := strParam(op.Params, "mode")
	if mode == "" {
		mode = "nat_1to1"
	}

	target := strParam(op.Params, "vm_ip")
	if target == "" && vmName != "" {
		target = vmIP(ctx, vmName)
	}
	uplink := defaultRouteIface(ctx)
	isV6 := strings.Contains(ip, ":")

	if isV6 {
		// IPv6 没有 1:1 NAT：地址足够多，控制面只应提供 routed/bridged。
		switch action {
		case "bind", "migrate":
			if mode == "nat_1to1" || mode == "nat" || mode == "" {
				return failResult("IPv6 公网地址不支持 1:1 NAT，请使用 routed 或 bridged 模式"), nil
			}
			if mode == "routed" {
				if target == "" {
					return failResult("经典路由要求来宾已配置公网地址且能被节点解析到 IP"), nil
				}
				if _, err := sys.Run(ctx, "ip", "-6", "route", "replace", ip+"/128", "via", target); err != nil {
					return failResult("添加 IPv6 路由失败: " + err.Error()), nil
				}
				if uplink != "" {
					_, _ = sys.Run(ctx, "ip", "-6", "neigh", "replace", "proxy", ip, "dev", uplink)
				}
			}
		case "unbind":
			removeRoutedPublicIP(ctx, ip, uplink)
		default:
			return failResult("未知公网地址动作: " + action), nil
		}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PublicIPDataKey: agent.PublicIPInfo{Message: "IPv6 公网地址规则已更新"},
		}}, nil
	}

	switch action {
	case "bind", "migrate":
		switch mode {
		case "nat_1to1", "nat":
			if target == "" || target == "0.0.0.0/0" {
				return failResult("虚拟机当前没有可用的 IP（未开机或未获取地址），无法绑定公网地址"), nil
			}
			// 切模式时先清掉另一种模式可能留下的规则。
			removeRoutedPublicIP(ctx, ip, uplink)
			iptablesNatDel(ctx, "PREROUTING", "-d", ip, "-j", "DNAT", "--to-destination", target)
			iptablesNatAdd(ctx, "PREROUTING", "-d", ip, "-j", "DNAT", "--to-destination", target)
		case "routed":
			if target == "" || target == "0.0.0.0/0" {
				return failResult("经典路由要求来宾已配置公网地址且能被节点解析到 IP"), nil
			}
			// 路由模式不做地址转换，只把公网地址路由到来宾，并让上行
			// 网卡代答 ARP。
			iptablesNatDelMatching(ctx, "PREROUTING", func(fields []string) bool {
				return fieldsHave(fields, "-d", ip, true) && fieldsHave(fields, "-j", "DNAT", false)
			})
			_, _ = sys.Run(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1")
			if _, err := sys.Run(ctx, "ip", "route", "replace", ip+"/32", "via", target); err != nil {
				return failResult("添加路由失败: " + err.Error()), nil
			}
			if uplink != "" {
				_, _ = sys.Run(ctx, "ip", "neigh", "replace", "proxy", ip, "dev", uplink)
			}
		case "bridged":
			// 桥接模式的地址直接出现在来宾网卡上，宿主机不做 NAT/路由。
			// 能走到这里说明该 VM 的网卡已经接在同一二层网络（控制面前置）。
			iptablesNatDelMatching(ctx, "PREROUTING", func(fields []string) bool {
				return fieldsHave(fields, "-d", ip, true) && fieldsHave(fields, "-j", "DNAT", false)
			})
			removeRoutedPublicIP(ctx, ip, uplink)
		default:
			return failResult("不支持的公网地址模式: " + mode), nil
		}
	case "unbind":
		iptablesNatDelMatching(ctx, "PREROUTING", func(fields []string) bool {
			return fieldsHave(fields, "-d", ip, true) && fieldsHave(fields, "-j", "DNAT", false)
		})
		removeRoutedPublicIP(ctx, ip, uplink)
	default:
		return failResult("未知公网地址动作: " + action), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PublicIPDataKey: agent.PublicIPInfo{Message: "公网地址规则已更新"},
	}}, nil
}

// removeRoutedPublicIP 清掉 routed 模式留下的路由与 proxy ARP/NDP。
func removeRoutedPublicIP(ctx context.Context, ip, uplink string) {
	if strings.Contains(ip, ":") {
		_, _ = sys.Run(ctx, "ip", "-6", "route", "del", ip+"/128")
		if uplink != "" {
			_, _ = sys.Run(ctx, "ip", "-6", "neigh", "del", "proxy", ip, "dev", uplink)
		}
		return
	}
	_, _ = sys.Run(ctx, "ip", "route", "del", ip+"/32")
	if uplink != "" {
		_, _ = sys.Run(ctx, "ip", "neigh", "del", "proxy", ip, "dev", uplink)
	}
}

// iptablesNatDelMatching 从 iptables-save 里找 chain 上满足谓词的规则并
// 逐条 -D（按字段重建参数——iptables-save 会把地址规范化成 ip/prefix，
// 子串匹配会漏）。谓词在字段数组上判断，避免 "-d 1.2.3.4" 吃掉 "1.2.3.45"。
func iptablesNatDelMatching(ctx context.Context, chain string, match func(fields []string) bool) {
	out, err := sys.Run(ctx, "iptables-save", "-t", "nat")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "-A "+chain+" ") {
			continue
		}
		fields := strings.Fields(line)
		if !match(fields) {
			continue
		}
		fields[0] = "-D"
		_, _ = sys.Run(ctx, "iptables", append([]string{"-t", "nat"}, fields...)...)
	}
}

// fieldsHave 报告 fields 里是否存在「flag 后紧跟 value」的对；prefixOK 时
// value 匹配该前缀（iptables-save 的 ip/32 形态）。
func fieldsHave(fields []string, flag, value string, prefixOK bool) bool {
	for i, f := range fields {
		if f != flag || i+1 >= len(fields) {
			continue
		}
		v := fields[i+1]
		if v == value || (prefixOK && strings.HasPrefix(v, value+"/")) {
			return true
		}
	}
	return false
}

// portSecurityPrecheck 实现 port_security.precheck（只读探测）。
//
// 此前 agent 没有实现这个操作，而控制面 Apply 的第一步就是它——整个
// 端口安全功能经 API 完全不可用（真机抓出）。探测三件事：端口在不在
// OVS 上、流表工具可用性、将写入的流（人可读）。
func portSecurityPrecheck(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "probe", Name: "探测端口与流表", Index: 1, Total: 1})
	portRef := strParam(op.Params, "port_ref")
	spoofing := boolParam(op.Params, "spoofing_guard")
	isolation := boolParam(op.Params, "isolation")

	caps := []agent.PortSecurityCapability{}
	okOVS := false
	if v, _ := sys.OVSVersion(ctx); v != "" {
		okOVS = true
	}
	caps = append(caps, agent.PortSecurityCapability{
		Key: "ovs", Label: "Open vSwitch", Required: true, Missing: !okOVS,
		Reason: "未安装 Open vSwitch", Fix: "安装 openvswitch 并启动 ovs-vswitchd",
	})
	bridge := ""
	if okOVS {
		if b, err := sys.OVSInterfaceBridge(ctx, portRef); err == nil {
			bridge = b
		}
	}
	onBridge := bridge != ""
	caps = append(caps, agent.PortSecurityCapability{
		Key: "port_on_ovs", Label: "端口已挂载在 OVS 桥上", Required: true, Missing: !onBridge,
		Reason: "端口不在任何 OVS 桥上", Fix: "把网口接到 OVS 交换机后再启用端口安全",
	})
	ofctlOK := sys.RunOK(ctx, "ovs-ofctl", "--version")
	caps = append(caps, agent.PortSecurityCapability{
		Key: "ofctl", Label: "ovs-ofctl 流表工具", Required: true, Missing: !ofctlOK,
		Reason: "ovs-ofctl 不可用", Fix: "安装完整的 openvswitch 工具集",
	})

	rules := []string{}
	if onBridge && ofctlOK {
		if isolation {
			rules = append(rules, "in_port="+portRef+",action=drop")
		} else if spoofing {
			if mac := macOf(ctx, portRef); mac != "" {
				rules = append(rules, "in_port="+portRef+",dl_src="+mac+",action=normal")
			} else {
				rules = append(rules, "# 读不到端口 MAC，防伪造流无法写入")
			}
		}
	}
	// 已有流会被 del-first 清掉——如实提示而不是事后惊讶。
	if onBridge && ofctlOK {
		if out, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "dump-flows", bridge,
			"in_port="+portRef); err == nil && strings.TrimSpace(out) != "" {
			caps[2].Missing = false
			return precheckResult(caps, rules, []string{
				"该网口已存在 " + strconv.Itoa(strings.Count(out, "\n")+1) + " 条流，应用时会被先清空再重写",
			}), nil
		}
	}
	return precheckResult(caps, rules, nil), nil
}

func precheckResult(caps []agent.PortSecurityCapability, rules, warnings []string) *agent.Result {
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortSecurityPrecheckKey: agent.PortSecurityPrecheck{
			Capabilities: caps, Rules: rules, Warnings: warnings,
		},
	}}
}

// portSecurityApply 实现 port_security.apply（OVS 流表： spoofing 防护 + 隔离）。
func portSecurityApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "flows", Name: "写入 OVS 流表", Index: 1, Total: 1})
	portRef := strParam(op.Params, "port_ref")
	spoofing := boolParam(op.Params, "spoofing_guard")
	isolation := boolParam(op.Params, "isolation")
	pps, _ := paramInt(op.Params, "pps_limit")

	bridge, err := sys.OVSInterfaceBridge(ctx, portRef)
	if err != nil {
		return failResult("端口不在 OVS 上: " + err.Error()), nil
	}
	// 先清该口上的既有流再按需写入（期望状态语义）：停用（两个开关全关）
	// 走到这里就是纯清理——此前只会加不会删，控制面「停用」后 OVS 里的
	// 防伪造/隔离流全部残留（review 抓出）。macOf 读不到 MAC 时报错而不是
	// 生成 dl_src= 的非法流。
	_, derr := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "del-flows", bridge,
		"in_port="+portRef)
	meterAction := ""
	if pps > 0 {
		// 每个端口一个稳定 meter id；crc32 碰撞概率足够低，且即使碰撞
		// 也只是两个端口共用同一个速率上限。
		id := crc32.ChecksumIEEE([]byte(portRef))%60000 + 1
		// 先删同 id 的旧 meter：只 add 时已存在的 meter 会让新 rate
		// 静默不生效。
		_, _ = sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "del-meter", bridge,
			fmt.Sprintf("meter=%d", id))
		_, merr := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-meter", bridge,
			fmt.Sprintf("meter=%d,pktps,band=type=drop,rate=%d", id, pps))
		if merr != nil && !strings.Contains(strings.ToLower(merr.Error()), "exist") {
			return failResult("写入 PPS 限速 meter 失败: " + merr.Error()), nil
		}
		meterAction = fmt.Sprintf("meter:%d,", id)
	}
	switch {
	case isolation:
		if _, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
			"in_port="+portRef+",priority=200,actions=drop"); err != nil {
			return failResult("写入隔离流表失败: " + err.Error()), nil
		}
	case spoofing:
		mac := macOf(ctx, portRef)
		if mac == "" {
			return failResult("读不到端口 MAC，无法写入防伪造流表"), nil
		}
		action := "normal"
		if meterAction != "" {
			action = meterAction + "normal"
		}
		if _, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
			"in_port="+portRef+",dl_src="+mac+",priority=100,actions="+action); err != nil {
			return failResult("写入防伪造流表失败: " + err.Error()), nil
		}
	case pps > 0:
		if _, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
			"in_port="+portRef+",priority=50,actions="+meterAction+"normal"); err != nil {
			return failResult("写入 PPS 限速流表失败: " + err.Error()), nil
		}
	default:
		if derr != nil {
			// 两个开关全关且清流也失败：端口上可能本来就没有 kc 流。
			// 静默即可——期望状态是「无规则」。
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortSecurityDataKey: agent.PortSecurityInfo{Applied: true, Message: "端口安全规则已更新"},
	}}, nil
}

// macOf 读网口 MAC。
func macOf(ctx context.Context, ifname string) string {
	if b, err := osReadFile("/sys/class/net/" + ifname + "/address"); err == nil {
		return strings.TrimSpace(b)
	}
	links, _ := sys.ListLinks(ctx)
	for _, l := range links {
		if l.IfName == ifname {
			return l.Address
		}
	}
	return ""
}

// —— 端口镜像 ——
//
// P2 缺陷（实机确认）：原实现只取 source_ports 的第一个元素、**从不读
// target_switches**，建出的 OVS Mirror 只有 select_src_port/select_dst_port
// 而**没有 output_port**——OVS 没有任何复制目标，一个报文都不会被复制
// （`ovs-vsctl list Mirror` 里 output_port 为空即可证），多源/方向/多目标
// 全部被静默忽略。
//
// 参考语义（docs/08-reference/qvmconsole/capabilities.md §端口镜像）是
// 「用 tc 复制一个或多个接口的入站或出站报文，经专用 veth 注入一个或多个
// 空交换机」。OVS 侧的等价做法：为每个目标交换机准备一条专用通路——
// 跨桥用 veth 对（一端接源桥、一端接目标桥），把**源桥那端**设为 Mirror 的
// output_port；目标与源同桥时改用 internal 端口（同桥把 veth 两端都接上
// 会成环，广播包会自我放大成风暴）。
//
// 现场落盘在 /run/kc-mirror-state-<name>.json：disable 与看门狗必须能
// **只**回收本次创建的东西（行名、端口、veth、两端网桥），否则同桥上的
// 其它镜像会被误删，或者留下一套没人认识的口子。

// mirrorRunDir 是镜像运行态文件的目录（/run 在 agent 重启间保留，整机重启
// 才清空）。抽成变量是为了让测试能指到临时目录。
var mirrorRunDir = "/run"

// mirrorKindVeth / mirrorKindInternal 标记端口类型：internal 端口只需
// del-port（OVS 自建的 netdev 随之消失），veth 还要 ip link del。
const (
	mirrorKindVeth     = "veth"
	mirrorKindInternal = "internal"
)

// mirrorPort 是本次创建的端口以及它挂在哪个 OVS 桥上。
type mirrorPort struct {
	Bridge string `json:"bridge"`
	Port   string `json:"port"`
	Kind   string `json:"kind"`
}

// mirrorState 是一次 enable 在节点上留下的完整现场，供精确回收。
type mirrorState struct {
	Name      string       `json:"name"`
	Source    string       `json:"source_bridge"`
	Direction string       `json:"direction"`
	Mirrors   []string     `json:"mirrors"`
	Ports     []mirrorPort `json:"ports"`
	Veths     []string     `json:"veths"`
	CreatedAt int64        `json:"created_at"`
}

// mirrorRowPrefix 是 Mirror 行名的前缀（旧版单行镜像名 = 前缀本身）。
func mirrorRowPrefix(name string) string { return "kc-mir-" + name }

// mirrorRowName 是第 idx 个目标对应的 Mirror 行名。
func mirrorRowName(name string, idx int) string {
	return mirrorRowPrefix(name) + "-" + strconv.Itoa(idx)
}

// mirrorPortBase 返回某个目标交换机的端口名前缀。
//
// Linux ifname 上限 15 字符，因此固定 "kcm" + 8 位 hex（由镜像名与目标
// 交换机稳定哈希得出），加上 a/b 后缀共 12/13 字符；哈希里带上目标名，
// 多目标时各自的端口名天然不冲突，重复 enable 也得到同一组名字（幂等）。
func mirrorPortBase(name, target string) string {
	return fmt.Sprintf("kcm%08x", crc32.ChecksumIEEE([]byte(name+"|"+target)))
}

// mirrorFileKey 把镜像名变成文件名安全的后缀（名字可能是任意字符）。
func mirrorFileKey(name string) string {
	if k := pathSafe(name); k != "" {
		return k
	}
	return fmt.Sprintf("h%08x", crc32.ChecksumIEEE([]byte(name)))
}

// mirrorStatePath / mirrorConfirmPath / mirrorWDFile 是镜像的三类运行态文件。
func mirrorStatePath(name string) string {
	return mirrorRunDir + "/kc-mirror-state-" + mirrorFileKey(name) + ".json"
}

func mirrorConfirmPath(name string) string {
	return mirrorRunDir + "/kc-mirror-confirm-" + mirrorFileKey(name)
}

func mirrorWDFile(name string) string {
	return mirrorRunDir + "/kc-mirror-wd-" + mirrorFileKey(name)
}

// stringListParam 把参数里的列表解成去重后的字符串切片。
//
// 控制面发的是 JSON 数组（channel 还原成 []any）；逗号分隔字符串是同一
// 语义的另一种历史形状，一并容忍——漏读一个元素就等于少镜像一个端口。
func stringListParam(raw any) []string {
	out := make([]string, 0, 4)
	add := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		for _, seen := range out {
			if seen == s {
				return
			}
		}
		out = append(out, s)
	}
	switch v := raw.(type) {
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range v {
			add(s)
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			add(s)
		}
	}
	return out
}

// mirrorPortRefs 生成 @p0,@p1… 形式的引用列表（引用列必须用 UUID）。
func mirrorPortRefs(count int) string {
	refs := make([]string, 0, count)
	for i := 0; i < count; i++ {
		refs = append(refs, "@p"+strconv.Itoa(i))
	}
	return strings.Join(refs, ",")
}

// normalizeMirrorDirection 把方向词表归一到 both/ingress/egress。
//
// 未知值按 both 处理：宁可多镜像一个方向（用户能看见并能关掉），也不要
// 因为一个拼写差异就静默只镜像一半。
func normalizeMirrorDirection(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "ingress", "in", "rx", "src":
		return "ingress"
	case "egress", "out", "tx", "dst":
		return "egress"
	default:
		return "both"
	}
}

// mirrorSourceBridge 解析全部源端口所在的 OVS 网桥。
//
// 一个 Mirror 只能挂在一座桥上，因此全部源端口必须同桥。跨桥源需要为每座
// 桥各建一个镜像，当前不支持——**明确失败**，而不是像缺陷版本那样只镜像
// 第一个口却报告成功。
func mirrorSourceBridge(ctx context.Context, sources []string) (string, error) {
	bridge, first := "", ""
	for _, s := range sources {
		br, err := sys.OVSInterfaceBridge(ctx, s)
		if err != nil {
			return "", fmt.Errorf("源端口 %s 不在任何 OVS 交换机上: %v", s, err)
		}
		if bridge == "" {
			bridge, first = br, s
			continue
		}
		if br != bridge {
			return "", fmt.Errorf(
				"源端口 %s 在交换机 %s，而 %s 在交换机 %s；跨交换机的源需要为每座桥各建一个镜像，当前不支持",
				s, br, first, bridge)
		}
	}
	if bridge == "" {
		return "", fmt.Errorf("缺少镜像源端口")
	}
	return bridge, nil
}

// mirrorValidateTargets 校验每个目标交换机都是**已存在**的 OVS 网桥。
//
// 不存在就明确失败：静默跳过等于建一个没有出口的镜像——正是本次缺陷的
// 形状（看起来成功、实际什么都不复制）。
func mirrorValidateTargets(ctx context.Context, targets []string) error {
	for _, t := range targets {
		if !sys.RunOK(ctx, "ovs-vsctl", "br-exists", t) {
			return fmt.Errorf("目标交换机 %s 不存在；镜像必须注入到一个已存在的空交换机", t)
		}
	}
	return nil
}

// portMirrorEnable 实现 port_mirror.enable（OVS mirror + 节点侧看门狗）。
//
// 顺序是语义的一部分：先解析并校验（失败不留痕）→ 清理同名旧拓扑（重复
// enable 不叠行）→ 建目标通路与 Mirror 行 → 落盘现场 → 布防看门狗。
func portMirrorEnable(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "preflight", Name: "解析镜像源与目标", Index: 1, Total: 3})
	name := strings.TrimSpace(strParam(op.Params, "name"))
	if name == "" {
		name = strings.TrimSpace(op.Target)
	}
	if name == "" {
		return failResult("缺少镜像名称"), nil
	}
	sources := stringListParam(op.Params["source_ports"])
	if len(sources) == 0 {
		return failResult("缺少镜像源端口"), nil
	}
	targets := stringListParam(op.Params["target_switches"])
	if len(targets) == 0 {
		return failResult("缺少镜像目标交换机：没有注入目标，OVS 不会把报文复制到任何地方"), nil
	}
	direction := normalizeMirrorDirection(strParam(op.Params, "direction"))

	srcBridge, err := mirrorSourceBridge(ctx, sources)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if err := mirrorValidateTargets(ctx, targets); err != nil {
		return failResult(err.Error()), nil
	}

	// 幂等：先清掉同名旧拓扑（含旧版单行 kc-mir-<name> 与它的端口/veth），
	// 否则重复 enable 会一条一条地叠 Mirror 行、留一串孤儿 veth。
	//
	// 清理会顺带删掉看门狗文件，因此先把原窗口读出来：修复重下发
	// （watchdog_seconds 为负）必须沿用原到期时刻，而不是把「限时镜像」
	// 变成永久镜像。
	priorDeadline, hadPriorWD := readMirrorWatchdog(name)
	cleanupMirror(ctx, name)

	report(Stage{Key: "create_mirror", Name: "建立镜像", Index: 2, Total: 3})
	st := mirrorState{
		Name: name, Source: srcBridge, Direction: direction, CreatedAt: timeNowSec(),
		Mirrors: []string{}, Ports: []mirrorPort{}, Veths: []string{},
	}
	for idx, target := range targets {
		if err := buildOneMirror(ctx, &st, idx, target, sources, direction); err != nil {
			// 失败不留半套拓扑：已建的 Mirror 行/端口/veth 全部回滚。
			// 回滚本身失败只记日志，但原始原因必须如实返回。
			msg := err.Error()
			if rbErrs := cleanupMirrorState(ctx, st); len(rbErrs) > 0 {
				log.Printf("[nodeagent] 镜像 %s 回滚时出错: %v", name, rbErrs)
				msg += "（回滚时出错: " + strings.Join(rbErrs, "; ") + "）"
			} else {
				msg += "（已回滚本次已创建的部分）"
			}
			return failResult(msg), nil
		}
	}
	if err := writeMirrorState(st); err != nil {
		msg := "写入镜像现场失败: " + err.Error()
		if rbErrs := cleanupMirrorState(ctx, st); len(rbErrs) > 0 {
			log.Printf("[nodeagent] 镜像 %s 回滚时出错: %v", name, rbErrs)
			msg += "（回滚时出错: " + strings.Join(rbErrs, "; ") + "）"
		}
		return failResult(msg), nil
	}

	report(Stage{Key: "watch_arm", Name: "布防看门狗", Index: 3, Total: 3})
	wd := 120
	switch v := op.Params["watchdog_seconds"].(type) {
	case float64:
		wd = int(v)
	case int:
		wd = v
	}
	_ = osRemove(mirrorConfirmPath(name))
	// 负数表示“修复重下发”，不布防新看门狗：修复的目标是恢复持续状态，
	// 而不是重新进入等待用户确认的窗口——但原有的自动撤销窗口要保住，
	// 否则一次修复就把「N 分钟自动撤销」变成了「永久复制」。
	if wd > 0 {
		armMirrorWatchdog(name, wd)
	} else if hadPriorWD {
		restoreMirrorWatchdog(name, priorDeadline)
	}

	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortMirrorDataKey: agent.PortMirrorInfo{
			WatchdogSeconds: wd, Applied: len(st.Mirrors),
			Message: fmt.Sprintf(
				"已建立 %d 条镜像（%d 个源端口 → %d 个目标交换机，方向 %s）；看门狗已同时在节点侧启动",
				len(st.Mirrors), len(sources), len(targets), direction),
		},
	}}, nil
}

// buildOneMirror 为一个目标交换机建立「输出端口 + Mirror 行」。
//
// 目标与源同桥 → internal 端口；不同桥 → veth 对。两种情况下 output_port
// 都指向**源桥那一端**，这样被复制的报文才会从源桥流向目标桥。
func buildOneMirror(
	ctx context.Context, st *mirrorState, idx int, target string,
	sources []string, direction string,
) error {
	base := mirrorPortBase(st.Name, target)
	output := base + "a"
	rowName := mirrorRowName(st.Name, idx)

	if target == st.Source {
		// 同桥：veth 两端都接同一座桥会成环（广播包自我放大），改用
		// internal 端口——它一样能被设成 Mirror 的 output_port。
		if _, err := sys.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", st.Source, output,
			"--", "set", "Interface", output, "type=internal"); err != nil {
			return fmt.Errorf("在交换机 %s 上创建镜像输出端口 %s 失败: %v", st.Source, output, err)
		}
		st.Ports = append(st.Ports, mirrorPort{
			Bridge: st.Source, Port: output, Kind: mirrorKindInternal,
		})
	} else {
		peer := base + "b"
		// 端口名由「镜像名 + 目标交换机」哈希而来，因此同名设备只可能是
		// 本镜像上次留下的（例如 agent 在落盘前被杀）。先尽力删掉它：
		// 否则 ip link add 会一直 EEXIST，这个镜像从此再也启不起来。
		_, _ = sys.Run(ctx, "ip", "link", "del", output)
		if _, err := sys.Run(ctx, "ip", "link", "add", output, "type", "veth", "peer", "name", peer); err != nil {
			return fmt.Errorf("创建镜像 veth 对 %s/%s 失败: %v", output, peer, err)
		}
		// 先记账再继续：veth 一建出来就必须能被回滚看见。
		st.Veths = append(st.Veths, output)
		if _, err := sys.Run(ctx, "ip", "link", "set", output, "up"); err != nil {
			return fmt.Errorf("拉起镜像 veth %s 失败: %v", output, err)
		}
		if _, err := sys.Run(ctx, "ip", "link", "set", peer, "up"); err != nil {
			return fmt.Errorf("拉起镜像 veth %s 失败: %v", peer, err)
		}
		if _, err := sys.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", st.Source, output); err != nil {
			return fmt.Errorf("把镜像 veth %s 接入交换机 %s 失败: %v", output, st.Source, err)
		}
		st.Ports = append(st.Ports, mirrorPort{
			Bridge: st.Source, Port: output, Kind: mirrorKindVeth,
		})
		if _, err := sys.Run(ctx, "ovs-vsctl", "--may-exist", "add-port", target, peer); err != nil {
			return fmt.Errorf("把镜像 veth %s 接入目标交换机 %s 失败: %v", peer, target, err)
		}
		st.Ports = append(st.Ports, mirrorPort{
			Bridge: target, Port: peer, Kind: mirrorKindVeth,
		})
	}

	// ovs-vsctl 的事务语义（OVS 3.3 真机逐一实测）：
	//   - create 没有 --may-exist（报 "no option"），幂等只能「先清后建」；
	//   - create 同事务内被引用必须带 --id=@m，且引用列要在 create 上
	//     直接内联（@m 不能出现在 record 位置）；
	//   - 引用列的值不能写端口名字符串（"vnet0 is not a valid UUID"），
	//     要先 --id=@p0 get Port 换成命名 UUID；
	//   - 单个 Mirror 只能有一个 output_port，因此每个目标一条行。
	args := make([]string, 0, 4*len(sources)+12)
	for i, s := range sources {
		args = append(args, "--", "--id=@p"+strconv.Itoa(i), "get", "Port", s)
	}
	args = append(args, "--", "--id=@o", "get", "Port", output)
	args = append(args, "--", "--id=@m", "create", "Mirror", "name="+rowName)
	// 方向映射：ingress 只镜像进入交换机的报文（select_src_port），
	// egress 只镜像离开交换机的报文（select_dst_port）。两个选择列都要
	// 包含**全部**源端口，否则多选里只有第一个口被复制。
	switch direction {
	case "ingress":
		args = append(args, "select_src_port="+mirrorPortRefs(len(sources)))
	case "egress":
		args = append(args, "select_dst_port="+mirrorPortRefs(len(sources)))
	default:
		args = append(args, "select_src_port="+mirrorPortRefs(len(sources)),
			"select_dst_port="+mirrorPortRefs(len(sources)))
	}
	args = append(args, "output_port=@o")
	args = append(args, "--", "add", "Bridge", st.Source, "mirrors", "@m")
	if _, err := sys.Run(ctx, "ovs-vsctl", args...); err != nil {
		return fmt.Errorf("建立镜像行 %s 失败: %v", rowName, err)
	}
	st.Mirrors = append(st.Mirrors, rowName)
	return nil
}

// writeMirrorState 落盘现场（0600：里面有内部端口与拓扑信息）。
func writeMirrorState(st mirrorState) error {
	blob, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_ = osMkdirAllErr(mirrorRunDir)
	return osWriteFile(mirrorStatePath(st.Name), blob, 0o600)
}

// readMirrorState 读回现场；文件缺失或损坏时返回 false（走兜底清理）。
func readMirrorState(name string) (mirrorState, bool) {
	raw, err := osReadFile(mirrorStatePath(name))
	if err != nil {
		return mirrorState{}, false
	}
	var st mirrorState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return mirrorState{}, false
	}
	if st.Name == "" {
		st.Name = name
	}
	return st, true
}

// pathSafe 报告名字是否只含文件名安全字符（用于拼进路径的设备/接口名）。
func pathSafe(name string) string {
	if name == "" {
		return ""
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return ""
		}
	}
	return name
}

// mirrorRowExists 按名字精确判定 Mirror 行是否存在（get 对不存在的行报错）。
func mirrorRowExists(ctx context.Context, name string) bool {
	_, err := sys.Run(ctx, "ovs-vsctl", "get", "Mirror", name, "name")
	return err == nil
}

// mirrorRowNames 列出全部 Mirror 行名（多目标镜像一个目标一行，回收必须
// 按前缀找齐，不能只认一个固定名字）。
func mirrorRowNames(ctx context.Context) []string {
	out, err := sys.Run(ctx, "ovs-vsctl", "--format=csv", "--data=bare",
		"--no-heading", "--columns=name", "list", "Mirror")
	if err != nil {
		return nil
	}
	names := make([]string, 0, 4)
	for _, line := range strings.Split(out, "\n") {
		if s := strings.Trim(strings.TrimSpace(line), `"`); s != "" {
			names = append(names, s)
		}
	}
	return names
}

// mirrorRowNamesFor 返回属于镜像 name 的全部 Mirror 行名。
func mirrorRowNamesFor(ctx context.Context, name string) []string {
	rows := make([]string, 0, 2)
	for _, row := range mirrorRowNames(ctx) {
		if mirrorRowMatches(name, row) {
			rows = append(rows, row)
		}
	}
	return rows
}

// mirrorRowMatches 判定某个 Mirror 行名是否属于镜像 name。
//
// 匹配规则：旧版单行名（kc-mir-<name>）或「前缀 + 纯数字下标」。下标必须
// 全是数字，否则名叫 web 的镜像会把名叫 web-2 的镜像的行也当成自己的
// ——删别人的镜像比留一点残留糟得多。
func mirrorRowMatches(name, row string) bool {
	prefix := mirrorRowPrefix(name)
	if !strings.HasPrefix(row, prefix) {
		return false
	}
	rest := strings.TrimPrefix(row, prefix)
	if rest == "" {
		return true
	}
	if !strings.HasPrefix(rest, "-") {
		return false
	}
	idx := rest[1:]
	if idx == "" {
		return false
	}
	for _, r := range idx {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isMirrorPlumbingPort 判定端口名是否为本功能建立的镜像通路端口
// （kcm + 8 位 hex + a/b）。状态文件缺失时靠它尽力清理。
func isMirrorPlumbingPort(port string) bool {
	if len(port) != 13 || !strings.HasPrefix(port, "kcm") {
		return false
	}
	if last := port[12]; last != 'a' && last != 'b' {
		return false
	}
	for _, r := range port[3:11] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// sweepMirrorPorts 是**状态文件缺失**时的尽力清理：扫遍所有桥，把符合
// 镜像端口命名规律的端口摘掉（veth 端再删设备）。不报错——调用方是
// disable 与看门狗，它们的目标是「尽力恢复干净」，而不是复述历史。
func sweepMirrorPorts(ctx context.Context) {
	brs, _ := sys.OVSListBridges(ctx)
	for _, br := range brs {
		ports, err := sys.OVSPortsOfBridge(ctx, br)
		if err != nil {
			continue
		}
		for _, p := range ports {
			if !isMirrorPlumbingPort(p) {
				continue
			}
			_, _ = sys.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", br, p)
			_, _ = sys.Run(ctx, "ip", "link", "del", p)
		}
	}
}

// mirrorPresentRow 报告镜像 name 在 OVS 上是否有行，并给出用于展示的行名。
//
// 旧版单行名（kc-mir-<name>）与多目标行名（kc-mir-<name>-<i>）都算存在：
// 自检据此判断「配置是否还在」，只认一种形状就会把另一形状报成丢失。
func mirrorPresentRow(ctx context.Context, name string) (string, bool) {
	if legacy := mirrorRowPrefix(name); mirrorRowExists(ctx, legacy) {
		return legacy, true
	}
	if rows := mirrorRowNamesFor(ctx, name); len(rows) > 0 {
		return strings.Join(rows, ", "), true
	}
	return "", false
}

// cleanupMirrorState 按现场回收一条镜像的全部东西（回滚与撤销共用）。
//
// 顺序有语义：先摘 Mirror 行（行里的 output_port 指向下面要删的端口），
// 再 del-port，最后删 veth。返回值为失败项（调用方决定记日志还是上报）。
func cleanupMirrorState(ctx context.Context, st mirrorState) []string {
	errs := make([]string, 0, 2)
	for _, row := range st.Mirrors {
		detachMirror(ctx, row)
	}
	for _, p := range st.Ports {
		if p.Bridge == "" || p.Port == "" {
			continue
		}
		_, _ = sys.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", p.Bridge, p.Port)
	}
	// internal 端口随 del-port 一起消失（由 OVS 创建与销毁）；veth 必须
	// 显式 ip link del，删一端即删两端。
	for _, v := range st.Veths {
		if _, err := sys.Run(ctx, "ip", "link", "del", v); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "cannot find") {
				errs = append(errs, "删除 veth "+v+" 失败: "+err.Error())
			}
		}
	}
	return errs
}

// cleanupMirror 是本名前缀下镜像拓扑的**唯一**回收路径（disable、看门狗、
// 重建前的清理都走这里）。
//
// 状态缺失时（旧版本建的镜像、或状态文件被清掉）按行名前缀摘行并扫端口
// 兜底；无论状态如何，最后都按前缀再摘一遍行——状态与实际不一致时这是
// 唯一兜底，且前缀精确到「名字 + 分隔符」，同桥其它镜像不受影响。
func cleanupMirror(ctx context.Context, name string) {
	if st, ok := readMirrorState(name); ok {
		if errs := cleanupMirrorState(ctx, st); len(errs) > 0 {
			log.Printf("[nodeagent] 回收镜像 %s 时出错: %v", name, errs)
		}
	} else {
		sweepMirrorPorts(ctx)
	}
	for _, row := range mirrorRowNamesFor(ctx, name) {
		detachMirror(ctx, row)
	}
	_ = osRemove(mirrorStatePath(name))
	_ = osRemove(mirrorConfirmPath(name))
	_ = osRemove(mirrorWDFile(name))
}

// mirrorWDFile 是镜像看门狗的持久化文件（首行 = 到期 unix 秒，次行 = 镜像名）。
//
// 看门狗定时器在内存里，agent 重启即丢——镜像复制的是全量流量，残留
// 是隐私问题而不只是清洁问题。/run 在 agent 重启间保留（重启整机才清空），
// 启动时 rearmMirrorWatchdogs 据此补上定时器。名字写进文件是为了让
// 文件名被规范化（含非法字符的名字走哈希）后仍能还原出真正的镜像名。
func armMirrorWatchdog(name string, seconds int) {
	restoreMirrorWatchdog(name, timeNowSec()+int64(seconds))
}

// restoreMirrorWatchdog 按**给定到期时刻**布防（修复重下发与 agent 重启
// 都走这里：窗口是原来那个，不能重新计时）。
func restoreMirrorWatchdog(name string, deadline int64) {
	body := fmt.Sprintf("%d\n%s", deadline, name)
	_ = osWriteFile(mirrorWDFile(name), []byte(body), 0o600)
	remaining := deadline - timeNowSec()
	if remaining < 0 {
		remaining = 0 // 已过期：立刻补撤销（agent 停机期间错过的窗口）
	}
	schedule(int(remaining), func() { fireMirrorWatchdog(name) })
}

// parseMirrorWDFile 解析看门狗文件：首行到期时刻，次行镜像名（旧格式没有次行）。
func parseMirrorWDFile(raw string) (deadline int64, name string, ok bool) {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) == 0 {
		return 0, "", false
	}
	d, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return 0, "", false
	}
	if len(lines) > 1 {
		name = strings.TrimSpace(lines[1])
	}
	return d, name, true
}

// readMirrorWatchdog 读回未了结的看门狗（没有或内容损坏时 ok=false）。
func readMirrorWatchdog(name string) (deadline int64, ok bool) {
	raw, err := osReadFile(mirrorWDFile(name))
	if err != nil {
		return 0, false
	}
	deadline, _, ok = parseMirrorWDFile(raw)
	return deadline, ok
}

// fireMirrorWatchdog 是到期的动作：已确认则跳过，否则整套撤销；两种情况
// 都清掉持久化文件（到期后文件只剩历史价值，留着会让下次重启重复撤销）。
func fireMirrorWatchdog(name string) {
	defer func() { _ = osRemove(mirrorWDFile(name)) }()
	if _, err := osStat(mirrorConfirmPath(name)); err == nil {
		return
	}
	ctx2, cancel2 := contextWithTimeout(5 * timeSecond)
	defer cancel2()
	cleanupMirror(ctx2, name)
}

// rearmMirrorWatchdogs 在 agent 启动时恢复未了结的看门狗（已到期即刻补撤销）。
func rearmMirrorWatchdogs() {
	files, _ := filepath.Glob(mirrorRunDir + "/kc-mirror-wd-*")
	for _, f := range files {
		raw, err := osReadFile(f)
		if err != nil {
			continue
		}
		deadline, name, ok := parseMirrorWDFile(raw)
		if !ok {
			_ = osRemove(f) // 内容损坏：文件已不可信，撤销由控制面对账兜底
			continue
		}
		if name == "" {
			// 旧格式只有到期时刻：名字退回用文件名（规范化后的名字）。
			name = strings.TrimPrefix(filepath.Base(f), "kc-mirror-wd-")
		}
		restoreMirrorWatchdog(name, deadline)
	}
}

// detachMirror 把名为 name 的 Mirror 从所有桥上摘挂。
//
// Bridge.mirrors 是弱引用：摘掉**最后一个**引用后 Mirror 行被自动删除
// （真机实测）。挂载中的行对 `destroy` 静默无效（退出码还是 0），因此
// 摘挂是撤销镜像的唯一正确路径。行不存在时 get 报错、事务整体无效，
// 各桥尝试彼此独立、互不影响。
func detachMirror(ctx context.Context, name string) {
	brs, _ := sys.OVSListBridges(ctx)
	for _, br := range brs {
		_, _ = sys.Run(ctx, "ovs-vsctl", "--", "--id=@m", "get", "Mirror", name, "_uuid",
			"--", "remove", "Bridge", br, "mirrors", "@m")
	}
}

func portMirrorConfirm(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	name := mirrorOperationName(op)
	_ = osWriteFile(mirrorConfirmPath(name), []byte("ok"), 0o644)
	// 确认即取消看门狗——持久化文件也一并清掉，agent 重启后不得重臂。
	_ = osRemove(mirrorWDFile(name))
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortMirrorDataKey: agent.PortMirrorInfo{Applied: 1, Message: "看门狗已取消，镜像将保持生效"},
	}}, nil
}

// mirrorOperationName 取本次操作对应的镜像名（控制面把镜像名放在 Target
// 与 params.name 两处；两者都缺失时返回空串，由调用方如实失败/空操作）。
func mirrorOperationName(op agent.Operation) string {
	if n := strings.TrimSpace(strParam(op.Params, "name")); n != "" {
		return n
	}
	return strings.TrimSpace(op.Target)
}

func portMirrorDisable(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "disable", Name: "撤销镜像", Index: 1, Total: 1})
	name := mirrorOperationName(op)
	if name == "" {
		return failResult("缺少镜像名称，无法撤销"), nil
	}
	// 统一走 cleanupMirror：摘本名前缀下的全部 Mirror 行（同桥其它镜像不受
	// 影响）→ 从桥上摘掉记录在案的端口 → 删 veth → 清运行态文件。
	cleanupMirror(ctx, name)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortMirrorDataKey: agent.PortMirrorInfo{Applied: 0, Message: "镜像已撤销并清理运行态"},
	}}, nil
}

// networkCapture 实现一次限时抓包（tcpdump 落盘，控制面稍后 fetch）。
func networkCapture(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	iface := strParam(op.Params, "interface")
	// iface 直接拼进落盘路径（也传给 tcpdump -i）：带 / 或 .. 是路径穿越，
	// 拒绝而不是落盘到 captures 之外。
	if iface != pathSafe(iface) {
		return failResult("网口名非法: " + iface), nil
	}
	duration := 30
	if v, ok := op.Params["duration_sec"].(float64); ok && int(v) > 0 {
		duration = int(v)
	}
	report(Stage{Key: "tcpdump", Name: "抓包中", Index: 1, Total: 2})
	path := fmt.Sprintf("/var/lib/k_cockpit/captures/%s-%d.pcap", iface, timeNowSec())
	osMkdirAll("/var/lib/k_cockpit/captures")
	cmd := exec.CommandContext(ctx, "tcpdump", "-i", iface, "-w", path, "-U")
	if err := cmd.Start(); err != nil {
		return failResult("tcpdump 启动失败: " + err.Error()), nil
	}
	timer := schedule(duration, func() {
		_ = cmd.Process.Kill()
	})
	go func() {
		_ = cmd.Wait()
		timer.Stop()
	}()
	st, _ := os.Stat(path)
	report(Stage{Key: "write_file", Name: "落盘", Index: 2, Total: 2})
	// tcpdump 在后台限时运行：此刻文件还是空的，如实说明「进行中」，
	// 控制面按 FilePath 稍后取回（报「完成」+0 字节会让用户以为坏了）。
	return &agent.Result{Success: true, Data: map[string]any{
		agent.CaptureDataKey: agent.CaptureInfo{
			FilePath: path, SizeBytes: fileSizeOr0(st),
			Message: fmt.Sprintf("抓包进行中（约 %d 秒后可取回）", duration),
		},
	}}, nil
}

// vmInitReady 实现 vm.init.ready（F-2-17）。
//
// 判定依据是来宾里的 cloud-init 完成标记 `/var/lib/cloud/instance/boot-finished`：
// 它在 cloud-init 全部阶段结束后才被写出，比"拿到了 DHCP 地址"或"域已运行"
// 都更晚，因此可以作为弹出安装介质的安全信号。
//
// 访问来宾文件需要 qemu-guest-agent。没有 agent 时**如实报未就绪**，而不是
// 猜一个时间后弹出——弹早了会让还在安装的 WinPE / 安装器丢失介质。
func vmInitReady(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "init_ready", Name: "检查初始化完成标记", Index: 1, Total: 1})
	info := agent.InitReadyInfo{Ready: false, Detail: "来宾内尚未安装或未连通 qemu-guest-agent，无法确认初始化是否完成"}

	if guestPing(ctx, op.Target) {
		const marker = "/var/lib/cloud/instance/boot-finished"
		out, err := agentCmd(ctx, op.Target, "guest-file-open", "path", marker, "mode", "r")
		if err == nil {
			var resp struct {
				Return int `json:"return"`
			}
			if json.Unmarshal([]byte(out), &resp) == nil && resp.Return > 0 {
				info.Ready = true
				info.Detail = "cloud-init 已完成（检测到 " + marker + "）"
				// 句柄必须关掉：每轮探测开一个不关，长时间运行会耗尽
				// guest agent 的文件句柄表。
				_, _ = agentCmd(ctx, op.Target, "guest-file-close", "handle", resp.Return)
			}
		}
		if !info.Ready {
			info.Detail = "尚未检测到 cloud-init 完成标记；ConfigDrive（Windows）需要来宾日志访问，暂不自动弹出安装介质"
		}
	}

	return &agent.Result{Success: true, Data: map[string]any{
		agent.InitReadyDataKey: info,
	}}, nil
}
