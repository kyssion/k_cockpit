// netinfo.go：网卡与地址探测（上行口候选、邻居表的来源）。
package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Link 是 ip -j link 的一条。
type Link struct {
	IfIndex   int      `json:"ifindex"`
	IfName    string   `json:"ifname"`
	Flags     []string `json:"flags"`
	OperState string   `json:"operstate"`
	Address   string   `json:"address"`
	Speed     int      `json:"speed,omitempty"`
	LinkType  string   `json:"link_type"`
}

// AddrInfo 是 ip -j addr 里一个网口的地址。
type AddrInfo struct {
	IfName    string   `json:"ifname"`
	Flags     []string `json:"flags"`
	OperState string   `json:"operstate"`
	Address   string   `json:"address"`
	Speed     int      `json:"speed,omitempty"`
	AddrInfos []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// ListLinks 枚举网口。
func ListLinks(ctx context.Context) ([]Link, error) {
	out, err := Run(ctx, "ip", "-j", "link")
	if err != nil {
		return nil, err
	}
	var links []Link
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, fmt.Errorf("ip -j link 解析失败: %w", err)
	}
	return links, nil
}

// ListAddrs 枚举网口与地址。
func ListAddrs(ctx context.Context) ([]AddrInfo, error) {
	out, err := Run(ctx, "ip", "-j", "addr")
	if err != nil {
		return nil, err
	}
	var addrs []AddrInfo
	if err := json.Unmarshal([]byte(out), &addrs); err != nil {
		return nil, fmt.Errorf("ip -j addr 解析失败: %w", err)
	}
	return addrs, nil
}

// LinkHasIP 报告网口上是否有任何 IPv4/IPv6 地址（管理口识别的关键信号）。
func LinkHasIP(addrs []AddrInfo, ifname string) bool {
	for _, a := range addrs {
		if a.IfName == ifname && len(a.AddrInfos) > 0 {
			return true
		}
	}
	return false
}

// LinkSpeedMbps 读网口速率；读不到（虚拟口常见）返回空串。
func LinkSpeedMbps(ctx context.Context, ifname string) string {
	b, err := osReadFile("/sys/class/net/" + ifname + "/speed")
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(b)
	if s == "" || s == "-1" {
		return ""
	}
	return s + "Mb/s"
}

// NeighborEntryLocal 是 ip neighbor 的一条（与 agent.NeighborEntry 分开：
// 本地结构不带控制面回填的字段）。
type NeighborEntryLocal struct {
	IP        string
	MAC       string
	Interface string
	State     string
}

// ListNeighbors 读取邻居表（ip neighbor show 的空格切分解析）。
//
// 输出行形如 "198.51.100.10 dev enp0s1 lladdr aa:bb:.. REACHABLE"。
func ListNeighbors(ctx context.Context) ([]NeighborEntryLocal, error) {
	out, err := Run(ctx, "ip", "neighbor", "show")
	if err != nil {
		return nil, err
	}
	return parseNeighbors(out)
}

// parseNeighbors 解析 ip neighbor 输出。
func parseNeighbors(out string) ([]NeighborEntryLocal, error) {
	var out_ []NeighborEntryLocal
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		e := NeighborEntryLocal{IP: fields[0]}
		for i := 1; i+1 < len(fields); i += 2 {
			switch fields[i] {
			case "dev":
				e.Interface = fields[i+1]
			case "lladdr":
				e.MAC = fields[i+1]
			}
		}
		// 状态是最后一个字段（FAILED/REACHABLE/STALE/…）。
		if last := fields[len(fields)-1]; last != e.Interface && last != e.MAC {
			e.State = strings.ToLower(last)
		}
		if e.IP != "" {
			out_ = append(out_, e)
		}
	}
	return out_, nil
}

// BridgeOfInterface 解析网口挂在哪张桥上：OVS 口走 ovs-vsctl iface-to-br，
// 内核桥走 /sys/class/net/<if>/brdev；都查不到返回空。
func BridgeOfInterface(ctx context.Context, ifname string) string {
	if br, err := Run(ctx, "ovs-vsctl", "iface-to-br", ifname); err == nil {
		if s := strings.TrimSpace(br); s != "" && !strings.Contains(s, "no bridge") {
			return s
		}
	}
	if b, err := osReadFile("/sys/class/net/" + ifname + "/brdev"); err == nil {
		return strings.TrimSpace(b)
	}
	return ""
}

// IsPhysicalLike 报告网口是否像物理上行口（排除 lo / OVS 内部 / 虚拟机
// vnet / 已有桥 / 无线虚拟口）。候选列表的意义是把「可控的选择」限定在
// 真实存在的物理口上。
func IsPhysicalLike(name string) bool {
	if name == "lo" {
		return false
	}
	for _, prefix := range []string{"ovs", "vnet", "virbr", "br-", "tap", "docker", "veth", "lo"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}
