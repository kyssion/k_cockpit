package nodeagent

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

func TestVPACLPreviewRendersAndWarns(t *testing.T) {
	res, err := vmVPACLPreview(context.Background(), agent.Operation{
		Params: map[string]any{"spec": []agent.VpcACLRuleSpec{
			{Priority: 100, Action: "deny", Direction: "in", Protocol: "all"},
			{Priority: 200, Action: "allow", Direction: "in", Protocol: "tcp", DstCIDR: "10.0.0.0/24", PortStart: 22},
		}},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("preview 应成功: err=%v res=%+v", err, res)
	}
	p, ok := res.Data[agent.VpcACLPreviewDataKey].(agent.VpcACLPreview)
	if !ok {
		t.Fatalf("结果类型不对: %#v", res.Data)
	}
	if p.Count != 2 || len(p.Rendered) != 2 || p.Version == "" {
		t.Errorf("渲染结果不对: %+v", p)
	}
	if len(p.Warnings) == 0 {
		t.Error("deny-all 后面还有规则时应给出遮蔽告警")
	}
}

func TestIsDenyAll(t *testing.T) {
	if !isDenyAll(agent.VpcACLRuleSpec{Action: "deny", Protocol: "all"}) {
		t.Error("空 CIDR + all 应判定为拒绝全部")
	}
	if isDenyAll(agent.VpcACLRuleSpec{Action: "deny", Protocol: "tcp", PortStart: 22}) {
		t.Error("限定端口的规则不应判定为拒绝全部")
	}
	if isDenyAll(agent.VpcACLRuleSpec{Action: "allow", Protocol: "all"}) {
		t.Error("allow 不应判定为拒绝全部")
	}
}

func TestNormalizedCIDR(t *testing.T) {
	for _, s := range []string{"", "any", "0.0.0.0/0", "::/0"} {
		if got := normalizedCIDR(s); got != "" {
			t.Errorf("%q 应归一为空，得到 %q", s, got)
		}
	}
	if got := normalizedCIDR("10.0.0.0/24"); got != "10.0.0.0/24" {
		t.Errorf("CIDR 被改写: %q", got)
	}
}

func TestDefaultRouteIface(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"ip": "#!/bin/sh\ncase \"$*\" in\n  *\"route show default\"*) echo 'default via 192.0.2.1 dev eth9' ;;\n  *) exit 0 ;;\nesac\n",
	})
	if got := defaultRouteIface(context.Background()); got != "eth9" {
		t.Fatalf("默认路由网卡解析失败: %q", got)
	}
}

func TestPublicIPv6Detect(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"ip": `#!/bin/sh
case "$*" in
  *"-6 addr show"*) printf '2: eth0: <BROADCAST>\n    inet6 2001:db8:1::1/64 scope global\n    inet6 fe80::1/64 scope link\n3: eth1: <BROADCAST>\n    inet6 2001:db8:2::1/64 scope global\n' ;;
  *"-6 route show default"*) echo 'default via fe80::1 dev eth0' ;;
  *) exit 0 ;;
esac
`,
	})
	res, err := vmPublicIPv6Detect(context.Background(), agent.Operation{}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("detect 应成功: err=%v res=%+v", err, res)
	}
	rows, ok := res.Data[agent.IPv6PrefixDataKey].([]agent.IPv6PrefixInfo)
	if !ok || len(rows) != 2 {
		t.Fatalf("前缀数量不对: %#v", res.Data)
	}
	if rows[0].Prefix != "2001:db8:1::/64" || !rows[0].Trusted || rows[0].EgressIf != "eth0" {
		t.Errorf("第一个前缀不对: %+v", rows[0])
	}
	if rows[1].Trusted {
		t.Errorf("eth1 没有默认路由，不应标记 trusted: %+v", rows[1])
	}
}

func TestPublicIPPreviewWithoutVM(t *testing.T) {
	res, err := vmPublicIPPreview(context.Background(), agent.Operation{
		Target: "203.0.113.10",
		Params: map[string]any{"mode": "nat"},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("preview 应成功: err=%v res=%+v", err, res)
	}
	p, ok := res.Data[agent.PublicIPPreviewKey].(agent.PublicIPPreview)
	if !ok || len(p.Warnings) == 0 || len(p.Added) != 0 {
		t.Fatalf("没有虚拟机时应给出告警: %#v", res.Data)
	}
}

func TestGlobalBandwidthApplies(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"ip": "#!/bin/sh\ncase \"$*\" in\n  *\"route show default\"*) echo 'default via 192.0.2.1 dev eth9' ;;\n  *) exit 0 ;;\nesac\n",
		"tc": "#!/bin/sh\nexit 0\n",
	})
	res, err := vmNetworkGlobalBandwidth(context.Background(), agent.Operation{
		Params: map[string]any{"mbps": float64(100), "burst_mbps": float64(10)},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("带宽下发应成功: err=%v res=%+v", err, res)
	}
	info, ok := res.Data[agent.GlobalBandwidthDataKey].(agent.GlobalBandwidthInfo)
	if !ok || !info.Applied || info.Mbps != 100 || info.BurstMbps != 10 {
		t.Fatalf("结果不对: %#v", res.Data)
	}
}

func TestCaptureFetchRejectsOutsidePath(t *testing.T) {
	res, err := vmCaptureFetch(context.Background(), agent.Operation{
		Params: map[string]any{"file_path": "/etc/passwd"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "非法") {
		t.Fatalf("目录外路径应拒绝: %+v", res)
	}
}

func TestCaptureDeleteRejectsOutsidePath(t *testing.T) {
	res, err := vmNetworkCaptureDelete(context.Background(), agent.Operation{
		Params: map[string]any{"file_path": "/etc/passwd"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "拒绝") {
		t.Fatalf("目录外路径应拒绝: %+v", res)
	}
}
