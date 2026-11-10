package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// TestNetworkBridgeApplyHonorsBackend 控制面把 backend 下发到节点，节点必须
// 照着建：记录写 ovs 却在宿主机上建出 Linux 桥，是第 12 轮发现的假选项。
func TestNetworkBridgeApplyHonorsBackend(t *testing.T) {
	for _, tc := range []struct {
		backend  string
		want     string
		unwanted string
	}{
		{"ovs", "ovs --may-exist add-br br-x", "ip link add name br-x type bridge"},
		{"bridge", "ip link add name br-x type bridge", "ovs --may-exist add-br"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "calls.log")
			installFakeCommands(t, map[string]string{
				"ip":        "#!/bin/sh\necho \"ip $*\" >> " + logPath + "\nexit 0\n",
				"ovs-vsctl": "#!/bin/sh\necho \"ovs $*\" >> " + logPath + "\nexit 0\n",
			})
			res, err := networkBridgeApply(context.Background(), agent.Operation{
				Target: "br-x", Params: map[string]any{"backend": tc.backend},
			}, func(Stage) {})
			if err != nil || !res.Success {
				t.Fatalf("建桥应成功: err=%v res=%+v", err, res)
			}
			buf, _ := os.ReadFile(logPath)
			if !strings.Contains(string(buf), tc.want) {
				t.Errorf("backend=%s 应执行 %q，实际命令: %s", tc.backend, tc.want, buf)
			}
			if strings.Contains(string(buf), tc.unwanted) {
				t.Errorf("backend=%s 不应执行 %q，实际命令: %s", tc.backend, tc.unwanted, buf)
			}
		})
	}
}

// TestNetworkBridgeDeleteByKind 删除请求只带桥名；OVS 桥与 Linux 桥的
// 拆除命令不同，删错了会留下孤儿桥或误删同名设备。
func TestNetworkBridgeDeleteByKind(t *testing.T) {
	for _, tc := range []struct {
		name       string
		brExistsRC int
		want       string
		unwanted   string
	}{
		{"ovs", 0, "ovs --if-exists del-br br-x", "ip link del br-x"},
		{"linux", 1, "ip link del br-x", "del-br br-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := "0"
			if tc.brExistsRC != 0 {
				rc = "1"
			}
			logPath := filepath.Join(t.TempDir(), "calls.log")
			installFakeCommands(t, map[string]string{
				"ip": "#!/bin/sh\necho \"ip $*\" >> " + logPath + "\nexit 0\n",
				"ovs-vsctl": "#!/bin/sh\n" +
					"echo \"ovs $*\" >> " + logPath + "\n" +
					"case \"$*\" in *\"br-exists\"*) exit " + rc + " ;; esac\n" +
					"exit 0\n",
			})
			res, err := networkBridgeDelete(context.Background(),
				agent.Operation{Target: "br-x"}, func(Stage) {})
			if err != nil || !res.Success {
				t.Fatalf("删桥应成功: err=%v res=%+v", err, res)
			}
			buf, _ := os.ReadFile(logPath)
			if !strings.Contains(string(buf), tc.want) {
				t.Errorf("应执行 %q，实际命令: %s", tc.want, buf)
			}
			if strings.Contains(string(buf), tc.unwanted) {
				t.Errorf("不应执行 %q，实际命令: %s", tc.unwanted, buf)
			}
		})
	}
}

func TestParseDHCPRange(t *testing.T) {
	start, end := parseDHCPRange("dhcp-range=10.0.0.100,10.0.0.200,12h")
	if start != "10.0.0.100" || end != "10.0.0.200" {
		t.Fatalf("解析结果 %q %q", start, end)
	}
	if s, e := parseDHCPRange("dhcp-range=broken"); s != "" || e != "" {
		t.Fatalf("非法行应返回空: %q %q", s, e)
	}
}

func TestFilterHostLines(t *testing.T) {
	got := filterHostLines([]string{
		"interface=br0", "dhcp-host=52:54:00:aa:bb:01,10.0.0.10",
		"dhcp-range=10.0.0.100,10.0.0.200,12h",
	})
	if len(got) != 1 || got[0] != "dhcp-host=52:54:00:aa:bb:01,10.0.0.10" {
		t.Fatalf("只应保留 dhcp-host 行: %#v", got)
	}
}

// 网关地址必须带上 CIDR 的前缀长度。
//
// 不带前缀时 `ip addr add 10.77.0.1 dev kbrX` 会落成 /32，内核不会为该网段
// 建 connected 路由（实机：ip route 里查不到 10.77.0.0/24，虚拟机连网关都不通）。
func TestCIDRPrefixLen(t *testing.T) {
	cases := map[string]string{
		"10.77.0.0/24":   "/24",
		"192.168.5.0/16": "/16",
		"10.0.0.0/8":     "/8",
		"":               "",
		"not-a-cidr":     "",
	}
	for in, want := range cases {
		if got := cidrPrefixLen(in); got != want {
			t.Errorf("cidrPrefixLen(%q) = %q, 期望 %q", in, got, want)
		}
	}
}
