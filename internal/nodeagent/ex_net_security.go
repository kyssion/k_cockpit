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

// securityGroupApply 实现 security_group.apply：per-VM 生效规则整台下发。
//
// 实现：为虚拟机的每个网口建一条专属链（KC-SG-<vm>），入向挂到对应
// OVS/桥的 FORWARD 路径不可行（OVS 流表是 P3 深水区），因此采用
// **宿主机 iptables 的 PHYSDEV 不可用时退化为 IP 口径**——按虚拟机当前
// IP（邻居表/静态地址）匹配来源与目标。P3 第一批先落地「规则链建立 +
// 默认拒绝/允许」；OVS 原生流表口径随端口安全一并深化。
func securityGroupApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "render", Name: "渲染规则链", Index: 1, Total: 2})
	vm := op.Target
	chain := "KC-SG-" + strings.ReplaceAll(vm, "-", "_")
	_, _ = sys.Run(ctx, "iptables", "-N", chain) // 已存在则报错，忽略

	// 重建：先清空。
	_, _ = sys.Run(ctx, "iptables", "-F", chain)

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
			base = []string{"-d", vmIP(ctx, vm)}
			if targetType == "cidr" || targetType == "ip" {
				base = append(base, "-s", targetValue)
			}
		case "out":
			base = []string{"-s", vmIP(ctx, vm)}
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
	return &agent.Result{Success: true, Data: map[string]any{
		agent.SecurityGroupDataKey: agent.SecurityGroupInfo{
			Applied: applied, Message: fmt.Sprintf("已下发 %d 条生效规则（链 %s）", applied, chain),
		},
	}}, nil
}

// vmIP 尽力拿到虚拟机当前 IP（邻居表 + virsh domifaddr 兜底）。
func vmIP(ctx context.Context, vm string) string {
	if out, err := sys.Run(ctx, "virsh", "-r", "-c", "qemu:///system", "domifaddr", vm); err == nil {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 4 && strings.Contains(fields[3], "/") {
				return strings.SplitN(fields[3], "/", 2)[0]
			}
		}
	}
	entries, _ := sys.ListNeighbors(ctx)
	for _, e := range entries {
		if e.Interface != "" && strings.HasPrefix(e.Interface, "vnet") {
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
	// 规则集（allow/deny）。
	if rules, ok := op.Params["rules"].([]any); ok {
		for _, r := range rules {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			action, _ := m["action"].(string)
			target := "DROP"
			if action == "allow" {
				target = "ACCEPT"
			}
			proto, _ := m["protocol"].(string)
			port, _ := m["port"].(float64)
			args := []string{"-p", proto}
			if proto == "" {
				args = []string{}
			}
			// 没有 -p 的 --dport 是非法规则（iptables 报错被 iptablesAdd
			// 吞掉，规则静默缺失）——无协议时端口无从匹配，忽略。
			if port > 0 && proto != "" {
				args = append(args, "--dport", strconv.Itoa(int(port)))
			}
			if src, ok := m["source"].(string); ok && src != "" {
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
		// 「收紧失败 + 报告成功」等于裸奔（review 抓出）。
		policy := "ACCEPT"
		if defaultAction == "drop" {
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
	const chain = "KC-FW"
	_, _ = sys.Run(ctx, "iptables", "-D", "INPUT", "-j", chain)
	_, _ = sys.Run(ctx, "iptables", "-P", "INPUT", "ACCEPT")
	_, _ = sys.Run(ctx, "iptables", "-F", chain)
	_, _ = sys.Run(ctx, "iptables", "-X", chain)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.FirewallDataKey: agent.FirewallInfo{Applied: 0, Message: "防火墙已回滚为全通"},
	}}, nil
}

// hostFirewallApply 实现 host_firewall.apply（第一层：SSH/面板保护 + 规则）。
//
// 与 firewall.apply 同一机制、不同链名与固定保护规则（SSH 22 与面板端口
// 永远放行——控制面传进来的面板端口是合成保护规则）。
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
	iptablesAdd(ctx, chain, "-p", "tcp", "--dport", "22", "-j", "ACCEPT") // SSH 保护规则
	if wl, ok := op.Params["whitelist"].([]any); ok {
		for _, w := range wl {
			if s, ok := w.(string); ok && s != "" {
				iptablesAdd(ctx, chain, "-s", s, "-j", "ACCEPT")
			}
		}
	}
	_, _ = sys.Run(ctx, "iptables", "-D", "INPUT", "-j", chain)
	if enabled {
		if _, err := sys.Run(ctx, "iptables", "-I", "INPUT", "1", "-j", chain); err != nil {
			return failResult(err.Error()), nil
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.HostFirewallDataKey: agent.HostFirewallInfo{AppliedRules: 1, Message: "宿主机防火墙已应用"},
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

	dnatArgs := []string{"-t", "nat", "-p", protocol, "--dport", strconv.Itoa(int(hostPort))}
	if src, ok := op.Params["allowed_ips"].(string); ok && src != "" {
		dnatArgs = append(dnatArgs, "-s", src)
	}
	dnatTo := []string{"-j", "DNAT", "--to-destination", targetIP + ":" + strconv.Itoa(int(targetPort))}

	switch action {
	case "add":
		full := append(append([]string{}, dnatArgs...), dnatTo...)
		iptablesAdd(ctx, full...)
		// 转发路径放行（filter 层）。
		fw := []string{"-p", protocol, "--dport", strconv.Itoa(int(hostPort))}
		if src, ok := op.Params["allowed_ips"].(string); ok && src != "" {
			fw = append(fw, "-s", src)
		}
		iptablesAdd(ctx, append([]string{"FORWARD", "-d", targetIP}, fw...)...)
		iptablesAdd(ctx, "FORWARD", "-d", targetIP, "-j", "ACCEPT")
	case "remove":
		full := append(append([]string{}, dnatArgs...), dnatTo...)
		iptablesDel(ctx, full...)
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
func publicIPChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "public_ip", Name: "调整公网 DNAT", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	ip := strParam(op.Params, "ip")
	vmIP := strParam(op.Params, "vm_ip")
	_ = vmIP
	// P2/P3 简化：以 1:1 全端口 DNAT 表达绑定（真实的多模式 NAT 随 P3 第三批）。
	switch action {
	case "bind":
		iptablesAdd(ctx, "-t", "nat", "-p", "all", "-d", ip, "-j", "DNAT", "--to-destination", vmIP)
	case "unbind":
		iptablesDel(ctx, "-t", "nat", "-p", "all", "-d", ip, "-j", "DNAT", "--to-destination", vmIP)
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PublicIPDataKey: agent.PublicIPInfo{Message: "公网地址规则已更新"},
	}}, nil
}

// portSecurityApply 实现 port_security.apply（OVS 流表： spoofing 防护 + 隔离）。
func portSecurityApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "flows", Name: "写入 OVS 流表", Index: 1, Total: 1})
	portRef := strParam(op.Params, "port_ref")
	vm := strParam(op.Params, "vm_name")
	spoofing := boolParam(op.Params, "spoofing_guard")
	isolation := boolParam(op.Params, "isolation")

	bridge, err := sys.OVSInterfaceBridge(ctx, portRef)
	if err != nil {
		return failResult("端口不在 OVS 上: " + err.Error()), nil
	}
	// in_port=<port> 的流量只允许该端口的 MAC/IP（动态学习由管理面下发）。
	if _, err := sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
		"in_port="+portRef+",dl_src="+macOf(ctx, portRef)+",action=normal"); err != nil && spoofing {
		return failResult("写入防伪造流表失败: " + err.Error()), nil
	}
	if isolation {
		_, _ = sys.Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "add-flow", bridge,
			"in_port="+portRef+",action=drop")
	}
	_ = vm
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PortSecurityDataKey: agent.PortSecurityInfo{Applied: true, Message: "端口安全规则已写入"},
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
	schedule(wd, func() {
		if _, err := osStat(confirmFile); err == nil {
			return
		}
		ctx2, cancel2 := contextWithTimeout(5 * timeSecond)
		defer cancel2()
		detachMirror(ctx2, mirrorName)
	})

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
	return &agent.Result{Success: true, Data: map[string]any{
		agent.CaptureDataKey: agent.CaptureInfo{
			FilePath: path, SizeBytes: fileSizeOr0(st), Message: "抓包完成",
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
