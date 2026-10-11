// ex_net_write.go：网络域的写操作执行器（P3 第一批——VPC 交换机（OVS）、
// 网桥（Linux 桥）、上行口入桥（带节点侧看门狗）、网络收敛修复）。
//
// 实现口径与真机环境对齐：OVS 3.3（ovs-vsctl/ovs-ofctl）、iproute2、
// dnsmasq。安全组/防火墙/公网 IP/端口安全/镜像/抓包随 P3 第二批。
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
			_, _ = sys.Run(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING",
				"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
			if _, err := sys.Run(ctx, "iptables", "-t", "nat", "-C", "POSTROUTING",
				"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE"); err == nil {
				// 已存在。
			} else {
				_, _ = sys.Run(ctx, "iptables", "-t", "nat", "-A", "POSTROUTING",
					"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
			}
		}
		// 带宽总限（OVS ingress policing 简化实现：仅入向）。
		if spec.BandwidthIn > 0 {
			_, _ = sys.Run(ctx, "ovs-vsctl", "set", "interface", bridge,
				"ingress_policing_rate="+strconv.Itoa(spec.BandwidthIn*1000))
		}

	case "delete":
		report(Stage{Key: "destroy", Name: "拆除交换机", Index: 1, Total: 1})
		stopDnsmasqForBridge(bridge)
		_, _ = sys.Run(ctx, "ip", "link", "del", bridge) // OVS 桥删除后 internal 口随之消失
		if _, err := sys.Run(ctx, "ovs-vsctl", "--if-exists", "del-br", bridge); err != nil {
			return failResult("删除 OVS 桥失败: " + err.Error()), nil
		}

	default:
		return failResult("未知交换机动作: " + action), nil
	}
	return &agent.Result{Success: true}, nil
}

// vpcSwitchAction 实现 migrate / reconfigure / release_port。
func vpcSwitchAction(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	switch action {
	case "reconfigure":
		// 幂等收敛：按 spec 重新走一遍 create 路径（OVS/dnsmasq 全部幂等）。
		return vpcSwitchChange(ctx, agent.Operation{
			Kind: agent.OpVPCSwitchChange, Target: op.Target, Params: map[string]any{
				"action": "create", "spec": op.Params["spec"],
			},
		}, report)
	case "migrate":
		return failResult("交换机迁移（换物理口）需要无上行口冲突校验，随 P3 第二批"), nil
	case "release_port":
		port := strParam(op.Params, "port")
		if port != "" {
			_, _ = sys.Run(ctx, "ovs-vsctl", "--if-exists", "del-port", op.Target, port)
		}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PortReleaseDataKey: agent.PortReleaseInfo{Released: port != "", Message: "端口已释放"},
		}}, nil
	}
	return failResult("未知交换机动作: " + action), nil
}

// runDnsmasqForBridge 在桥上启动独立 dnsmasq（pid 文件幂等：已跑则先停）。
func runDnsmasqForBridge(ctx context.Context, bridge, start, end string) error {
	pidFile := "/run/kc-dnsmasq-" + bridge + ".pid"
	stopDnsmasqForBridge(bridge)
	cmd := exec.CommandContext(ctx, "dnsmasq", "--no-daemon", "--pid-file="+pidFile,
		"--interface="+bridge, "--bind-dynamic", /* 只服务该桥上的请求 */
		"--dhcp-range="+start+","+end+",12h", "--dhcp-option=option:router,"+gatewayGuess(start),
		"--no-hosts", "--no-resolv", "--log-facility=/dev/null")
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不等退出：dnsmasq --no-daemon 会一直跑；交给系统，ctx 取消时kill。
	go func() { _ = cmd.Wait() }()
	return nil
}

// stopDnsmasqForBridge 停掉桥对应的 dnsmasq。
func stopDnsmasqForBridge(bridge string) {
	b, err := osReadFile("/run/kc-dnsmasq-" + bridge + ".pid")
	if err != nil {
		return
	}
	_ = exec.Command("kill", strings.TrimSpace(b)).Run()
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
