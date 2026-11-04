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
