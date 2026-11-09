package nodeagent

import "testing"

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
