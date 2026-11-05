package main

import "testing"

func TestDuplicatePrefixes(t *testing.T) {
	names := []string{
		"0017_security_group_multi.sql",
		"0018_bridge_uplink_watchdog.sql",
		"0018_drop_unused_security_challenge.sql",
		"0019_bridge_detail.sql",
		"0019_resource_quota.sql",
		"0020_host_firewall.sql",
	}
	got := duplicatePrefixes(names)
	if len(got) != 2 {
		t.Fatalf("应发现 2 组重复编号，实际 %d: %v", len(got), got)
	}
	if got[0][0] != "0018_bridge_uplink_watchdog.sql" || got[0][1] != "0018_drop_unused_security_challenge.sql" {
		t.Errorf("第一组不对: %v", got[0])
	}
	if got[1][0] != "0019_bridge_detail.sql" || got[1][1] != "0019_resource_quota.sql" {
		t.Errorf("第二组不对: %v", got[1])
	}
}

func TestDuplicatePrefixesEmptyWhenUnique(t *testing.T) {
	if got := duplicatePrefixes([]string{"0001_a.sql", "0002_b.sql", "0003_c.sql"}); len(got) != 0 {
		t.Errorf("编号唯一时不应报告重复: %v", got)
	}
}
