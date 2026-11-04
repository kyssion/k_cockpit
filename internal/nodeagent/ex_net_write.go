// ex_net_write.go：网络域的写操作执行器（P3 第一批——VPC 交换机（OVS）、
// 网桥（Linux 桥）、上行口入桥（带节点侧看门狗）、网络收敛修复）。
//
// 实现口径与真机环境对齐：OVS 3.3（ovs-vsctl/ovs-ofctl）、iproute2、
// dnsmasq。安全组/防火墙/公网 IP/端口安全/镜像/抓包在 ex_net_security.go
// 与 ex_net_extra.go。
package nodeagent

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// switchSpec 是 vpc.switch.change 的 spec 结构化视图。
type switchSpec struct {
	Name         string
	Mode         string
	VlanID       int
	CIDR         string
	GatewayIP    string
	DHCPStart    string
	DHCPEnd      string
	UplinkIf     string
	BandwidthIn  int
	BandwidthOut int
}

func parseSwitchSpec(params map[string]any) switchSpec {
	s := switchSpec{}
	if raw, ok := params["spec"].(map[string]any); ok {
		str := func(k string) string { v, _ := raw[k].(string); return v }
		num := func(k string) int { v, _ := raw[k].(float64); return int(v) }
		s.Name = str("name")
		s.Mode = str("mode")
		s.VlanID = num("vlan_id")
		s.CIDR = str("cidr")
		s.GatewayIP = str("gateway_ip")
		s.DHCPStart = str("dhcp_start")
		s.DHCPEnd = str("dhcp_end")
		s.UplinkIf = str("uplink_if")
		s.BandwidthIn = num("bandwidth_in_mbps")
		s.BandwidthOut = num("bandwidth_out_mbps")
	}
	return s
}

// vpcSwitchChange 实现 vpc.switch.change（create / update / delete）。
//
// OVS 桥 + internal 端口做网关 + 独立 dnsmasq（DHCP）+ NAT 出网。删除时
// 逆序拆除（杀 dnsmasq → 摘 internal 口 → 删桥）。
func vpcSwitchChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	bridge := op.Target
	action := strParam(op.Params, "action")
	spec := parseSwitchSpec(op.Params)

	switch action {
	case "create", "update":
		report(Stage{Key: "create_bridge", Name: "创建 OVS 桥", Index: 1, Total: 3})
		if _, err := sys.Run(ctx, "ovs-vsctl", "--may-exist", "add-br", bridge); err != nil {
			return failResult("创建 OVS 桥失败: " + err.Error()), nil
		}
		if spec.VlanID > 0 {
			_, _ = sys.Run(ctx, "ovs-vsctl", "set", "bridge", bridge, "other_config:vlan-mode=access")
		}

		report(Stage{Key: "gateway", Name: "配置网关与 DHCP", Index: 2, Total: 3})
		if spec.CIDR != "" {
			// internal 口做网关：ip addr add + link up（幂等：先删后加会闪断，
			// 用 addr replace）。
			gw := spec.GatewayIP
			if gw == "" {
				// CIDR 的第一个可用地址做网关。
				gw = firstAddrOf(spec.CIDR)
			}
			_, _ = sys.Run(ctx, "ip", "addr", "replace", gw, "dev", bridge)
			_, _ = sys.Run(ctx, "ip", "link", "set", bridge, "up")
			// DHCP：dnsmasq 绑定到该桥（listen 接口 + 动态范围）。
			if spec.DHCPStart != "" && spec.DHCPEnd != "" {
				if err := runDnsmasqForBridge(ctx, bridge, spec.DHCPStart, spec.DHCPEnd); err != nil {
					return failResult("DHCP 启动失败: " + err.Error()), nil
				}
			}
			// NAT：把该网段 MASQUERADE 到上行（幂等，已存在则跳过）。
			iptablesNatAdd(ctx, "POSTROUTING",
				"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
		}
		// 带宽总限（OVS ingress policing 简化实现：仅入向）。
		if spec.BandwidthIn > 0 {
			_, _ = sys.Run(ctx, "ovs-vsctl", "set", "interface", bridge,
				"ingress_policing_rate="+strconv.Itoa(spec.BandwidthIn*1000))
		}

	case "delete":
		report(Stage{Key: "destroy", Name: "拆除交换机", Index: 1, Total: 1})
		stopDnsmasqForBridge(bridge)
		_ = osRemove(dnsmasqConfPath(bridge))
		// NAT 的 MASQUERADE 随桥一起拆（真机验证抓出：此前只删桥，
		// POSTROUTING 里的规则残留——交换机没了规则还在，地址段被下一个
		// 交换机复用时会叠出双份规则）。
		if spec.CIDR != "" {
			_, _ = sys.Run(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING",
				"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
		}
		_, _ = sys.Run(ctx, "ip", "link", "del", bridge) // OVS 桥删除后 internal 口随之消失
		if _, err := sys.Run(ctx, "ovs-vsctl", "--if-exists", "del-br", bridge); err != nil {
			return failResult("删除 OVS 桥失败: " + err.Error()), nil
		}

	default:
		return failResult("未知交换机动作: " + action), nil
	}
	return &agent.Result{Success: true}, nil
}

// runDnsmasqForBridge 在桥上启动独立 dnsmasq（pid 文件幂等：已跑则先停）。
func runDnsmasqForBridge(_ context.Context, bridge, start, end string) error {
	// 保留已有静态租约：重配置交换机不能把用户绑定的静态地址清掉。
	hosts := readDnsmasqHosts(bridge)
	if err := writeDnsmasqConf(bridge, start, end, hosts); err != nil {
		return err
	}
	return startDnsmasqForBridge(bridge)
}

func dnsmasqConfPath(bridge string) string {
	return "/var/lib/k_cockpit/dnsmasq/" + bridge + ".conf"
}

// writeDnsmasqConf 写基础配置与静态租约。
func writeDnsmasqConf(bridge, start, end string, hosts []string) error {
	_ = osMkdirAllErr("/var/lib/k_cockpit/dnsmasq")
	var b strings.Builder
	fmt.Fprintf(&b, "interface=%s\n", bridge)
	b.WriteString("bind-dynamic\n")
	fmt.Fprintf(&b, "dhcp-range=%s,%s,12h\n", start, end)
	fmt.Fprintf(&b, "dhcp-option=option:router,%s\n", gatewayGuess(start))
	b.WriteString("no-hosts\nno-resolv\nlog-facility=/dev/null\n")
	for _, h := range hosts {
		if strings.TrimSpace(h) != "" {
			b.WriteString(strings.TrimSpace(h) + "\n")
		}
	}
	return osWriteFile(dnsmasqConfPath(bridge), []byte(b.String()), 0o600)
}

// readDnsmasqHosts 取配置里已有的 dhcp-host 行。
func readDnsmasqHosts(bridge string) []string {
	b, err := osReadFile(dnsmasqConfPath(bridge))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(b, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "dhcp-host=") {
			out = append(out, line)
		}
	}
	return out
}

// startDnsmasqForBridge 用配置文件启动 dnsmasq。
func startDnsmasqForBridge(bridge string) error {
	pidFile := "/run/kc-dnsmasq-" + bridge + ".pid"
	stopDnsmasqForBridge(bridge)
	// **刻意不用请求 ctx**：此前绑定在 gRPC 会话 ctx 上——agent 一断流
	// 重连，全部交换机的 DHCP 同时被杀且无人拉起（review 抓出）。DHCP 的
	// 生命周期属于桥，不属于某次下发；停机由 stopDnsmasqForBridge
	// （删除/重配置交换机的路径）负责。
	// 不给 dnsmasq 传 --pid-file：前台模式（--no-daemon）实测**不写 pid
	// 文件**（真机验证），靠它写等于没有——pid 由 agent 自己落盘。
	cmd := exec.Command("dnsmasq", "--no-daemon", "--conf-file="+dnsmasqConfPath(bridge))
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = osWriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	go func() { _ = cmd.Wait() }()
	return nil
}

// upsertDHCPHost 增加/替换一条 DHCP 静态租约并重启该桥的 dnsmasq。
//
// 配置文件不存在时用控制面下发的范围补建：交换机可能是旧版本 agent
// 创建的，当时还没有 conf 文件。
func upsertDHCPHost(ctx context.Context, bridge, mac, ip, start, end string) error {
	path := dnsmasqConfPath(bridge)
	b, err := osReadFile(path)
	if err != nil {
		if start == "" || end == "" {
			return fmt.Errorf("网桥 %s 没有由平台管理的 DHCP 配置: %w", bridge, err)
		}
		if err := writeDnsmasqConf(bridge, start, end, nil); err != nil {
			return err
		}
		b, err = osReadFile(path)
		if err != nil {
			return err
		}
	}
	want := "dhcp-host=" + strings.ToLower(mac) + "," + ip
	var lines []string
	for _, line := range strings.Split(b, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "dhcp-host=") &&
			(strings.Contains(strings.ToLower(trim), strings.ToLower(mac)+",") ||
				strings.HasSuffix(trim, ","+ip)) {
			continue
		}
		lines = append(lines, trim)
	}
	lines = append(lines, want)
	// 用原有的 dhcp-range 重写：从配置文本里取，避免再问控制面。
	rangeStart, rangeEnd := dhcpRangeOf(lines)
	if rangeStart == "" || rangeEnd == "" {
		return fmt.Errorf("网桥 %s 的 DHCP 配置缺少 dhcp-range", bridge)
	}
	if err := writeDnsmasqConf(bridge, rangeStart, rangeEnd, filterHostLines(lines)); err != nil {
		return err
	}
	return startDnsmasqForBridge(bridge)
}

// removeDHCPHost 删除一条静态租约；配置不存在时按幂等成功处理。
func removeDHCPHost(_ context.Context, bridge, mac string) error {
	path := dnsmasqConfPath(bridge)
	b, err := osReadFile(path)
	if err != nil {
		return nil
	}
	var kept []string
	start, end := "", ""
	for _, line := range strings.Split(b, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "dhcp-range=") {
			start, end = parseDHCPRange(trim)
			kept = append(kept, trim)
			continue
		}
		if strings.HasPrefix(trim, "dhcp-host=") &&
			strings.Contains(strings.ToLower(trim), strings.ToLower(mac)+",") {
			continue
		}
		kept = append(kept, trim)
	}
	if start == "" || end == "" {
		return nil
	}
	if err := writeDnsmasqConf(bridge, start, end, filterHostLines(kept)); err != nil {
		return err
	}
	return startDnsmasqForBridge(bridge)
}

func filterHostLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "dhcp-host=") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func parseDHCPRange(line string) (string, string) {
	v := strings.TrimPrefix(line, "dhcp-range=")
	parts := strings.Split(v, ",")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func dhcpRangeOf(lines []string) (string, string) {
	for _, line := range lines {
		if strings.HasPrefix(line, "dhcp-range=") {
			return parseDHCPRange(line)
		}
	}
	return "", ""
}

// stopDnsmasqForBridge 停掉桥对应的 dnsmasq。
//
// 杀前必须核验进程身份（AGENTS.md 杀进程纪律）：pid 文件在 /run，
// dnsmasq 崩溃后 PID 被系统复用时，裸 kill 会杀掉一个毫不相干的进程。
// /proc/<pid>/comm 不是 dnsmasq 一律不动（残留 pid 文件顺带清掉）。
func stopDnsmasqForBridge(bridge string) {
	pidFile := "/run/kc-dnsmasq-" + bridge + ".pid"
	b, err := osReadFile(pidFile)
	if err != nil {
		return
	}
	pid := strings.TrimSpace(b)
	if pid == "" {
		return
	}
	comm, cerr := osReadFile("/proc/" + pid + "/comm")
	if cerr != nil || strings.TrimSpace(comm) != "dnsmasq" {
		_ = osRemove(pidFile) // 陈旧 pid 文件：进程已不在或被复用，只清文件不杀
		return
	}
	_ = exec.Command("kill", pid).Run()
	_ = osRemove(pidFile)
}

// gatewayGuess 从 DHCP 起址猜网关（.1 惯例仅展示用；真实网关取自网关 IP 配置）。
func gatewayGuess(start string) string {
	parts := strings.Split(start, ".")
	if len(parts) != 4 {
		return start
	}
	return parts[0] + "." + parts[1] + "." + parts[2] + ".1"
}

// firstAddrOf 取 CIDR 的第一个可用主机地址做网关。
func firstAddrOf(cidr string) string {
	// 简化实现：10.0.0.0/24 → 10.0.0.1。
	base := strings.SplitN(cidr, "/", 2)[0]
	parts := strings.Split(base, ".")
	if len(parts) != 4 {
		return base
	}
	last, err := strconv.Atoi(parts[3])
	if err != nil {
		return base
	}
	parts[3] = strconv.Itoa(last + 1)
	return strings.Join(parts, ".")
}

// networkBridgeApply 实现 network.bridge.apply（Linux 基础桥）。
func networkBridgeApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "create", Name: "创建基础桥", Index: 1, Total: 1})
	name := op.Target
	if _, err := sys.Run(ctx, "ip", "link", "add", "name", name, "type", "bridge"); err != nil {
		// 已存在按成功（幂等收敛）。
		if !strings.Contains(err.Error(), "exist") {
			return failResult(err.Error()), nil
		}
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", name, "up")
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Fixed: []string{name}, Message: "基础桥已就绪"},
	}}, nil
}

// networkBridgeDelete 实现 network.bridge.delete。
func networkBridgeDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "delete", Name: "删除基础桥", Index: 1, Total: 1})
	if _, err := sys.Run(ctx, "ip", "link", "del", op.Target); err != nil {
		if !strings.Contains(err.Error(), "Cannot find") && !strings.Contains(err.Error(), "not found") {
			return failResult(err.Error()), nil
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Message: "基础桥已删除"},
	}}, nil
}

// networkUplinkAttach 实现 network.uplink.attach——**最危险操作**：
// 物理口入桥 + 节点侧看门狗（到点未确认自动摘出）。
func networkUplinkAttach(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	bridge := strParam(op.Params, "bridge")
	iface := strParam(op.Params, "uplink_if")
	seconds := 0
	if v, ok := op.Params["watchdog_seconds"].(float64); ok {
		seconds = int(v)
	}
	report(Stage{Key: "watch_arm", Name: "布防自动回滚看门狗", Index: 1, Total: 2})
	// **看门狗先布防再操作**（阶段顺序契约）：把摘口命令交给延时执行器，
	// 操作失败时主动解除。
	cancel := armUplinkWatchdog(bridge, iface, time.Duration(seconds)*time.Second)

	report(Stage{Key: "apply", Name: "物理口入桥", Index: 2, Total: 2})
	if _, err := sys.Run(ctx, "ip", "link", "set", iface, "nomaster"); err != nil {
		cancel()
		return failResult("物理口脱离原 master 失败: " + err.Error()), nil
	}
	if _, err := sys.Run(ctx, "ip", "link", "set", iface, "master", bridge); err != nil {
		cancel()
		return failResult("物理口入桥失败: " + err.Error()), nil
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", bridge, "up")
	// 看门狗保持布防：控制面在窗口内调 confirm 才解除。
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{
			Message: fmt.Sprintf("物理口 %s 已入桥 %s，看门狗 %d 秒", iface, bridge, seconds),
		},
	}}, nil
}

// armUplinkWatchdog 布防节点侧看门狗：到点未确认（confirm 文件未写）就
// 自动摘口。返回解除函数。
func armUplinkWatchdog(bridge, iface string, d time.Duration) (cancel func()) {
	confirmFile := "/run/kc-uplink-confirm-" + bridge
	_ = osRemove(confirmFile)
	t := time.AfterFunc(d, func() {
		if _, err := osStat(confirmFile); err == nil {
			return // 已确认
		}
		// 自动回滚：摘口。
		ctx, cancel2 := contextWithTimeout(5 * timeSecond)
		defer cancel2()
		_, _ = sys.Run(ctx, "ip", "link", "set", iface, "nomaster")
	})
	return func() {
		t.Stop()
		_ = osWriteFile(confirmFile, []byte("ok"), 0o644)
	}
}

// networkUplinkConfirm 实现 network.uplink.confirm（取消自动回滚）。
func networkUplinkConfirm(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "confirm", Name: "确认保持", Index: 1, Total: 1})
	_ = osWriteFile("/run/kc-uplink-confirm-"+op.Target, []byte("ok"), 0o644)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Message: "自动回滚已取消，物理口保持入桥"},
	}}, nil
}

// networkUplinkDetach 实现 network.uplink.detach（摘口并恢复）。
func networkUplinkDetach(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	iface := strParam(op.Params, "uplink_if")
	report(Stage{Key: "detach", Name: "摘出物理口", Index: 1, Total: 1})
	if _, err := sys.Run(ctx, "ip", "link", "set", iface, "nomaster"); err != nil {
		return failResult(err.Error()), nil
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", iface, "up")
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Message: "物理口已摘出并恢复 UP"},
	}}, nil
}

// networkRepair 实现 network.repair（幂等收敛：基础桥全部 up、OVS 服务在跑）。
func networkRepair(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "converge", Name: "核对网络现状", Index: 1, Total: 1})
	var fixed, remaining []string
	if !sys.OVSRunning(ctx) {
		if sys.RunOK(ctx, "systemctl", "start", "ovs-vswitchd") {
			fixed = append(fixed, "ovs-vswitchd 已拉起")
		} else {
			remaining = append(remaining, "ovs-vswitchd 无法启动（检查 OVS 安装）")
		}
	}
	links, _ := sys.ListLinks(ctx)
	for _, l := range links {
		if strings.HasPrefix(l.IfName, "br-") && l.OperState != "UP" {
			_, _ = sys.Run(ctx, "ip", "link", "set", l.IfName, "up")
			fixed = append(fixed, l.IfName+" 已置 UP")
		}
	}
	msg := "网络收敛完成"
	if len(remaining) > 0 {
		msg = "仍有需要人工处理的项目"
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Fixed: fixed, Remaining: remaining, Message: msg},
	}}, nil
}
