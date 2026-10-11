// ovs.go：Open vSwitch 探测（健康状态、端口清单）。
package sys

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// OVSVersion 返回 OVS 版本号；未安装返回空串与 nil（「缺失」不是错误）。
func OVSVersion(ctx context.Context) (string, error) {
	out, err := Run(ctx, "ovs-vsctl", "--version")
	if err != nil {
		return "", nil // 命令不存在 = 未安装
	}
	// 首行形如 "ovs-vsctl (Open vSwitch) 3.3.9"——只取首行末字段，
	// 第二行是 DB Schema 版本，混进来就是一个看起来对的错误数字。
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return "", nil
	}
	fields := strings.Fields(lines[0])
	if len(fields) > 0 {
		return fields[len(fields)-1], nil
	}
	return "", nil
}

// OVSRunning 报告 ovs-vswitchd 服务是否在跑。
//
// 「装了」与「在跑」分开是 OVSStatus 契约的要求：服务挂掉时探测仍会成功，
// 而所有依赖 OVS 的功能实际都已失效。
func OVSRunning(ctx context.Context) bool {
	return RunOK(ctx, "systemctl", "is-active", "--quiet", "ovs-vswitchd")
}

// OVSListBridges 列出 OVS 上的桥。
func OVSListBridges(ctx context.Context) ([]string, error) {
	out, err := Run(ctx, "ovs-vsctl", "list-br")
	if err != nil {
		return nil, nil // 未安装不是错误，返回空清单
	}
	var out_ []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out_ = append(out_, s)
		}
	}
	return out_, nil
}

// OVSPortsOfBridge 列出桥上的端口。
func OVSPortsOfBridge(ctx context.Context, bridge string) ([]string, error) {
	out, err := Run(ctx, "ovs-vsctl", "list-ports", bridge)
	if err != nil {
		return nil, err
	}
	var out_ []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out_ = append(out_, s)
		}
	}
	return out_, nil
}

// OVSPortInfo 取端口的类型与 VLAN tag（未打标为 0）。
func OVSPortInfo(ctx context.Context, bridge, port string) (typ string, tag int) {
	if out, err := Run(ctx, "ovs-vsctl", "get", "port", port, "type"); err == nil {
		typ = strings.Trim(strings.TrimSpace(out), `"`)
	}
	if out, err := Run(ctx, "ovs-vsctl", "get", "port", port, "tag"); err == nil {
		if v, perr := strconv.Atoi(strings.TrimSpace(out)); perr == nil {
			tag = v
		}
	}
	return typ, tag
}

// OVSFlowCount 统计桥上的流表条数（OpenFlow 1.3）。
//
// 同时兼任 OpenFlow 1.3 支持探测：命令失败即不支持——端口安全与镜像
// 依赖 1.3，这一条失败要让界面能看见。
func OVSFlowCount(ctx context.Context, bridge string) (int, bool) {
	out, err := Run(ctx, "ovs-ofctl", "-O", "OpenFlow13", "dump-flows", bridge)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " table=") {
			n++
		}
	}
	return n, true
}

// OVSMeterAvailable 报告 meter 表是否可用（包速率限制依赖它）。
func OVSMeterAvailable(ctx context.Context, bridge string) bool {
	return RunOK(ctx, "ovs-ofctl", "-O", "OpenFlow13", "meter-stats", bridge)
}

// OVSInterfaceBridge 解析 interface → bridge（vnet 口归属的桥）。
func OVSInterfaceBridge(ctx context.Context, ifname string) (string, error) {
	out, err := Run(ctx, "ovs-vsctl", "iface-to-br", ifname)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(out)
	if strings.Contains(s, "no bridge") {
		return "", fmt.Errorf("interface %s 不在任何 OVS 桥上", ifname)
	}
	return s, nil
}
