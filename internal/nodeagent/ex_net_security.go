// ex_net_security.go：网络安全类写执行器（P3 第二批）——安全组（per-VM
// iptables 链）、节点防火墙（基线 + 紧急回滚）、端口转发（DNAT）、
// 静态地址（dnsmasq host 条目）、公网地址（DNAT 池规则）、端口安全
// （OVS 流表）、端口镜像（OVS mirror）、抓包（tcpdump）。
package nodeagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// timeNowSec / fileSizeOr0 / osMkdirAll / fmtAnyString / timeTimer / schedule
// 是本文件用到的小助手。
func timeNowSec() int64 { return time.Now().Unix() }

func fileSizeOr0(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

func osMkdirAll(path string) { _ = os.MkdirAll(path, 0o755) }

func fmtAnyString(v any) string {
	s, _ := v.(string)
	return s
}

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
	// 静态地址的权威记录在控制面（static_ip 表）；节点侧在 DHCP 里做
	// MAC→IP 绑定需要 MAC（params 无 mac 字段——该参数形状为控制面记账
	// 为主，dnsmasq 绑定随 P3 第三批的交换机接入补齐）。
	return &agent.Result{Success: true, Message: "静态地址已在控制面登记（节点侧 DHCP 绑定随交换机接入补齐）"}, nil
}

// publicIPChange 实现 public.ip.change（bind/unbind/migrate 的 DNAT）。
//
// 目标 VM 的 IP 由节点解析：控制面的 executor 只发 vm_name、从不发
// vm_ip（真机 bind 报成功但 DNAT 0 条——空 --to-destination 的 iptables
// 报错被吞）。bind/migrate 解析不到 IP 时如实失败；unbind 时 VM 可能已
// 关机解析不到，按「-d <ip> + DNAT」的形状从规则集匹配删除，不要求重现
// 完整的 --to-destination。
func publicIPChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "public_ip", Name: "调整公网 DNAT", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	ip := strParam(op.Params, "ip")
	vmName := strParam(op.Params, "vm_name")

	switch action {
	case "bind", "migrate":
		target := strParam(op.Params, "vm_ip")
		if target == "" && vmName != "" {
			target = vmIP(ctx, vmName)
		}
		if target == "" || target == "0.0.0.0/0" {
			return failResult("虚拟机当前没有可用的 IP（未开机或未获取地址），无法绑定公网地址"), nil
		}
		iptablesNatDel(ctx, "PREROUTING", "-d", ip, "-j", "DNAT", "--to-destination", target)
		iptablesNatAdd(ctx, "PREROUTING", "-d", ip, "-j", "DNAT", "--to-destination", target)
	case "unbind":
		iptablesNatDelMatching(ctx, "PREROUTING", func(fields []string) bool {
			return fieldsHave(fields, "-d", ip, true) && fieldsHave(fields, "-j", "DNAT", false)
		})
	default:
		return failResult("未知公网地址动作: " + action), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PublicIPDataKey: agent.PublicIPInfo{Message: "公网地址规则已更新"},
	}}, nil
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
	switch {
	case isolation:
		_, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
			"in_port="+portRef+",action=drop")
		if err != nil {
			return failResult("写入隔离流表失败: " + err.Error()), nil
		}
	case spoofing:
		mac := macOf(ctx, portRef)
		if mac == "" {
			return failResult("读不到端口 MAC，无法写入防伪造流表"), nil
		}
		if _, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
			"in_port="+portRef+",dl_src="+mac+",action=normal"); err != nil {
			return failResult("写入防伪造流表失败: " + err.Error()), nil
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

// portMirrorEnable 实现 port_mirror.enable（OVS mirror + 节点侧看门狗）。
func portMirrorEnable(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	name := strParam(op.Params, "name")
	report(Stage{Key: "create_mirror", Name: "建立镜像", Index: 1, Total: 2})
	sources, _ := op.Params["source_ports"].([]any)
	if len(sources) == 0 {
		return failResult("缺少镜像源端口"), nil
	}
	bridge, err := sys.OVSInterfaceBridge(ctx, fmtAnyString(sources[0]))
	if err != nil {
		return failResult(err.Error()), nil
	}
	mirrorName := "kc-mir-" + name
	// ovs-vsctl 的事务语义（OVS 3.3 真机逐一实测）：
	//   - create 没有 --may-exist（报 "no option"），幂等只能「先查后建」；
	//   - create 同事务内被引用必须带 --id=@m，且引用列要在 create 上
	//     直接内联（@m 不能出现在 record 位置）；
	//   - 引用列的值不能写端口名字符串（"vnet0 is not a valid UUID"），
	//     要先 --id=@p0 get Port 换成命名 UUID；
	//   - 行已存在时 set 的 record 用名字、add 的值必须先 get _uuid。
	ensureMirror := func(source string) error {
		if mirrorRowExists(ctx, mirrorName) {
			args := []string{"--", "--id=@p0", "get", "Port", source,
				"--", "set", "Mirror", mirrorName,
				"select_src_port=@p0", "select_dst_port=@p0",
				"--", "--id=@m", "get", "Mirror", mirrorName, "_uuid",
				"--", "add", "Bridge", bridge, "mirrors", "@m"}
			_, err := sys.Run(ctx, "ovs-vsctl", args...)
			return err
		}
		args := []string{"--", "--id=@p0", "get", "Port", source,
			"--", "--id=@m", "create", "Mirror", "name=" + mirrorName,
			"select_src_port=@p0", "select_dst_port=@p0",
			"--", "add", "Bridge", bridge, "mirrors", "@m"}
		_, err := sys.Run(ctx, "ovs-vsctl", args...)
		return err
	}
	if err := ensureMirror(fmtAnyString(sources[0])); err != nil {
		return failResult("建立镜像失败: " + err.Error()), nil
	}

	report(Stage{Key: "watch_arm", Name: "布防看门狗", Index: 2, Total: 2})
	wd := 120
	if v, ok := op.Params["watchdog_seconds"].(float64); ok && int(v) > 0 {
		wd = int(v)
	}
	confirmFile := "/run/kc-mirror-confirm-" + name
	_ = osRemove(confirmFile)
	armMirrorWatchdog(name, mirrorName, wd)

	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortMirrorDataKey: agent.PortMirrorInfo{
			WatchdogSeconds: wd, Applied: 1,
			Message: "镜像已建立；看门狗已同时在节点侧启动",
		},
	}}, nil
}

// mirrorRowExists 按名字精确判定 Mirror 行是否存在（get 对不存在的行报错）。
func mirrorRowExists(ctx context.Context, name string) bool {
	_, err := sys.Run(ctx, "ovs-vsctl", "get", "Mirror", name, "name")
	return err == nil
}

// mirrorWDFile 是镜像看门狗的持久化文件（内容 = 到期 unix 秒）。
//
// 看门狗定时器在内存里，agent 重启即丢——镜像复制的是全量流量，残留
// 是隐私问题而不只是清洁问题。/run 在 agent 重启间保留（重启整机才清空），
// 启动时 rearmMirrorWatchdogs 据此补上定时器。
func mirrorWDFile(name string) string { return "/run/kc-mirror-wd-" + name }

// armMirrorWatchdog 布防一个看门狗：落盘到期时刻 + 内存定时器。
func armMirrorWatchdog(name, mirrorName string, seconds int) {
	_ = osWriteFile(mirrorWDFile(name), []byte(fmt.Sprintf("%d", timeNowSec()+int64(seconds))), 0o600)
	schedule(seconds, func() { fireMirrorWatchdog(name, mirrorName) })
}

// fireMirrorWatchdog 是到期的动作：已确认则跳过，否则摘挂；两种情况都
// 清掉持久化文件（到期后文件只剩历史价值，留着会让下次重启重复撤销）。
func fireMirrorWatchdog(name, mirrorName string) {
	defer func() { _ = osRemove(mirrorWDFile(name)) }()
	if _, err := osStat("/run/kc-mirror-confirm-" + name); err == nil {
		return
	}
	ctx2, cancel2 := contextWithTimeout(5 * timeSecond)
	defer cancel2()
	detachMirror(ctx2, mirrorName)
}

// rearmMirrorWatchdogs 在 agent 启动时恢复未了结的看门狗（已到期即刻补撤销）。
func rearmMirrorWatchdogs() {
	files, _ := filepath.Glob("/run/kc-mirror-wd-*")
	now := timeNowSec()
	for _, f := range files {
		b, err := osReadFile(f)
		if err != nil {
			continue
		}
		deadline, perr := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
		if perr != nil {
			_ = osRemove(f) // 内容损坏：文件已不可信，撤销由控制面对账兜底
			continue
		}
		name := strings.TrimPrefix(filepath.Base(f), "kc-mirror-wd-")
		remaining := deadline - now
		if remaining < 0 {
			remaining = 0 // 已过期：立刻补撤销（agent 停机期间错过的窗口）
		}
		schedule(int(remaining), func() { fireMirrorWatchdog(name, "kc-mir-"+name) })
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
	_ = osWriteFile("/run/kc-mirror-confirm-"+op.Target, []byte("ok"), 0o644)
	// 确认即取消看门狗——持久化文件也一并清掉，agent 重启后不得重臂。
	_ = osRemove(mirrorWDFile(op.Target))
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortMirrorDataKey: agent.PortMirrorInfo{Applied: 1, Message: "看门狗已取消，镜像将保持生效"},
	}}, nil
}

func portMirrorDisable(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "disable", Name: "撤销镜像", Index: 1, Total: 1})
	// 只摘本镜像（弱引用 GC 会随后删掉孤儿行），同桥其它镜像不受影响。
	detachMirror(ctx, "kc-mir-"+op.Target)
	_ = osRemove("/run/kc-mirror-confirm-" + op.Target)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortMirrorDataKey: agent.PortMirrorInfo{Applied: 0, Message: "镜像已撤销"},
	}}, nil
}

// networkCapture 实现一次限时抓包（tcpdump 落盘，控制面稍后 fetch）。
func networkCapture(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	iface := strParam(op.Params, "interface")
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

// vmInitReady 实现 vm.init.ready（P2 诚实占位：真实判定读来宾日志，P5）。
func vmInitReady(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	return &agent.Result{Success: true, Data: map[string]any{
		agent.InitReadyDataKey: agent.InitReadyInfo{Ready: false,
			Detail: "初始化判定需要来宾日志访问（P5 来宾自动化接入）"},
	}}, nil
}
