package hostfirewall_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/network/hostfirewall"
)

// 本文件覆盖「预览文案与节点实际规则不符」（实机复现）：
//
// 预览写着 `iptables -P INPUT DENY`，而节点既不改 INPUT 的默认策略、链尾也
// 没有兜底 DROP——于是在没有 ufw 的主机上，未匹配的流量照样放行，界面上却
// 写着「默认拒绝」。预览是用户按下「应用」前唯一看到的东西，它必须与节点
// 真正写下的规则一字不差。

// setPolicy 改策略并返回最新版本。
func setPolicy(t *testing.T, svc *hostfirewall.Service, req hostfirewall.PolicyRequest) {
	t.Helper()
	if _, err := svc.UpdatePolicy(context.Background(), 1, req, admin(), "root", ""); err != nil {
		t.Fatalf("更新策略失败: %v", err)
	}
}

// TestPrecheckPreviewShowsChainTailDrop 覆盖默认拒绝的预览。
func TestPrecheckPreviewShowsChainTailDrop(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	enabled := true
	wl := "192.168.64.1"
	setPolicy(t, svc, hostfirewall.PolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallDeny, Whitelist: &wl,
	})

	preview, warnings, err := svc.Precheck(ctx, 1)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	joined := strings.Join(preview, "\n")

	// 与节点侧一致：兜底是链尾规则。
	if !strings.Contains(joined, "KC-HFW -j DROP") {
		t.Fatalf("默认拒绝的预览必须写出链尾兜底 DROP，实际:\n%s", joined)
	}
	// 与节点侧一致：不改宿主机自己的默认策略（那会与 ufw 打架且无法随链回滚）。
	if strings.Contains(joined, "-P INPUT") {
		t.Errorf("预览不得声称修改 INPUT 默认策略——节点侧做的是链级兜底:\n%s", joined)
	}
	if strings.Contains(joined, "DENY") {
		t.Errorf("`DENY` 不是合法的 iptables 目标（DROP/ACCEPT 才是）:\n%s", joined)
	}
	// 兜底必须排在放行之后：顺序就是它的语义。
	last := strings.TrimSpace(preview[len(preview)-1])
	if !strings.Contains(last, "KC-HFW -j DROP") {
		t.Errorf("兜底 DROP 必须排在所有放行规则之后，实际末行: %q\n%s", last, joined)
	}

	// 默认拒绝会连 DHCP/DNS 一起拦掉：只说"可能影响网络"没有用，要能照做。
	warnJoined := strings.Join(warnings, " ")
	for _, want := range []string{"DHCP", "DNS", "67", "53"} {
		if !strings.Contains(warnJoined, want) {
			t.Errorf("警告应给出可执行的补救（缺 %q），实际: %v", want, warnings)
		}
	}
	if !strings.Contains(warnJoined, "放行") {
		t.Errorf("警告必须说明补救手段是显式放行规则，实际: %v", warnings)
	}
}

// TestPrecheckAcceptPreviewHasNoDropFallback 覆盖默认放行：没有兜底拒绝。
func TestPrecheckAcceptPreviewHasNoDropFallback(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	enabled := true
	wl := "192.168.64.1"
	setPolicy(t, svc, hostfirewall.PolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallAccept, Whitelist: &wl,
	})

	preview, warnings, err := svc.Precheck(ctx, 1)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	joined := strings.Join(preview, "\n")
	if strings.Contains(joined, "DROP") {
		t.Errorf("默认放行时不该出现兜底 DROP 行:\n%s", joined)
	}
	if strings.Contains(strings.Join(warnings, " "), "DHCP") {
		t.Errorf("默认放行时不该有 DHCP/DNS 警告: %v", warnings)
	}
}

// TestPrecheckDisabledDenyWarnsNoServiceImpact 覆盖启用开关：停用时链被摘出
// INPUT，DHCP/DNS 的拦截不会发生，此时不该再报这条警告（噪音会让人忽略真
// 正的警告）。
func TestPrecheckDisabledDenyWarnsNoServiceImpact(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	disabled := false
	wl := "192.168.64.1"
	setPolicy(t, svc, hostfirewall.PolicyRequest{
		Enabled: &disabled, DefaultAction: model.FirewallDeny, Whitelist: &wl,
	})

	_, warnings, err := svc.Precheck(ctx, 1)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if strings.Contains(strings.Join(warnings, " "), "DHCP") {
		t.Errorf("未启用时不该报 DHCP/DNS 拦截警告: %v", warnings)
	}
}
