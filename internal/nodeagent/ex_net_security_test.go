package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// 本文件覆盖宿主机防火墙「默认拒绝」不落地（实机复现）：
//
//	-P INPUT ACCEPT
//	-N KC-HFW
//	-A KC-HFW ... -j ACCEPT   （六条放行）
//
// 链尾没有兜底 DROP，INPUT 的默认策略也没被改过——于是在没有 ufw/既有 DROP
// 的主机上，任何未匹配的流量都放行，而界面与预览写着「默认拒绝」。
//
// 缺陷只在**命令层**可见（任务回报 success、节点命令序列看起来完整），因此
// 用可记账的假 iptables 钉住真实发出的命令：记录全部调用，并维护链的状态
// （-N/-F/-A/-C/-D），这样才能断言「链的最后一行是谁」以及「重复 apply 不
// 叠加」。

// fakeIptablesScript 是一个记账版 iptables：把每条调用写进日志，并按真实
// 语义维护每条链的规则列表（$KC_FAKE_IPTABLES_STATE/<chain>）。
//
// 必须模拟状态而不是"只记日志"：`-A/-C/-F` 的效果决定最终链内容，
// 只记日志的桩无法区分「重复 apply 叠加了两条 DROP」与「只有一条」。
const fakeIptablesScript = `#!/bin/sh
printf '%s\n' "$*" >> "$KC_FAKE_IPTABLES_LOG"
cmd=$1
chain=$2
[ -z "$cmd" ] && exit 1
shift 2 2>/dev/null || exit 1
spec="$*"
file="$KC_FAKE_IPTABLES_STATE/$chain"
case "$cmd" in
  -N)
    [ -f "$file" ] && exit 1
    : > "$file"
    exit 0
    ;;
  -F)
    [ -f "$file" ] || exit 1
    : > "$file"
    exit 0
    ;;
  -C)
    [ -f "$file" ] || exit 1
    grep -Fxq -- "$spec" "$file" || exit 1
    exit 0
    ;;
  -A)
    [ -f "$file" ] || : > "$file"
    printf '%s\n' "$spec" >> "$file"
    exit 0
    ;;
  -D)
    [ -f "$file" ] || exit 1
    grep -Fxq -- "$spec" "$file" || exit 1
    awk -v s="$spec" 'BEGIN{d=0} { if (!d && $0==s) { d=1; next } print }' "$file" > "$file.tmp"
    mv "$file.tmp" "$file"
    exit 0
    ;;
esac
exit 0
`

// fakeIptables 是一次测试里的假 iptables（自带状态目录与调用日志）。
type fakeIptables struct {
	t     *testing.T
	state string
	log   string
}

// installFakeIptables 把假 iptables 装到 PATH 最前面（不改生产代码：执行器
// 照常走 exec，只是解析到桩）。
func installFakeIptables(t *testing.T) *fakeIptables {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("建状态目录失败: %v", err)
	}
	stub := filepath.Join(dir, "iptables")
	if err := os.WriteFile(stub, []byte(fakeIptablesScript), 0o755); err != nil {
		t.Fatalf("写假 iptables 失败: %v", err)
	}
	f := &fakeIptables{t: t, state: state, log: filepath.Join(dir, "calls.log")}
	if err := os.WriteFile(f.log, nil, 0o644); err != nil {
		t.Fatalf("建调用日志失败: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KC_FAKE_IPTABLES_STATE", state)
	t.Setenv("KC_FAKE_IPTABLES_LOG", f.log)
	return f
}

// chain 返回某条链当前的规则列表（每条是 -A 之后的参数串，按写入顺序）。
func (f *fakeIptables) chain(name string) []string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.state, name))
	if err != nil {
		f.t.Fatalf("读链 %s 失败: %v（调用：\n%s）", name, err, f.calls())
	}
	out := []string{}
	for _, line := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// calls 返回全部被发出的命令（每条一行），失败时用于说明现场。
func (f *fakeIptables) calls() string {
	f.t.Helper()
	b, err := os.ReadFile(f.log)
	if err != nil {
		return "(读不到调用日志)"
	}
	return string(b)
}

// hasCall 报告是否发出过 given 前缀开头的命令。
func (f *fakeIptables) hasCall(prefix string) bool {
	for _, line := range strings.Split(f.calls(), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// countIn 统计链规则里与 want 完全相同的条数。
func countIn(rules []string, want string) int {
	n := 0
	for _, r := range rules {
		if r == want {
			n++
		}
	}
	return n
}

// hostFirewallOp 是控制面 Apply 下发的真实形状（见 hostfirewall.Service.Apply：
// whitelist 是逗号分隔字符串，规则键是 port_start/source_cidr）。
func hostFirewallOp(enabled bool, defaultAction string) agent.Operation {
	return agent.Operation{
		Kind:   agent.OpHostFirewallApply,
		NodeID: 1,
		Params: map[string]any{
			"action":         "apply",
			"enabled":        enabled,
			"default_action": defaultAction,
			"whitelist":      "192.168.64.1",
			"rules": []any{
				map[string]any{
					"action": "accept", "protocol": "tcp",
					"port_start": float64(18080), "is_protected": true,
				},
			},
		},
	}
}

func applyHostFirewall(t *testing.T, op agent.Operation) {
	t.Helper()
	res, err := hostFirewallApply(context.Background(), op, func(Stage) {})
	if err != nil {
		t.Fatalf("应用宿主机防火墙失败: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("应用应成功，实际 %#v", res)
	}
}

// TestHostFirewallDenyAppendsChainTailDrop 是缺陷的主症状：默认拒绝必须由
// KC-HFW 自己的链尾兜住。没有这一条时，实机上空有六条 ACCEPT 而 INPUT 仍是
// ACCEPT——「默认拒绝」只存在于文案里。
func TestHostFirewallDenyAppendsChainTailDrop(t *testing.T) {
	f := installFakeIptables(t)
	applyHostFirewall(t, hostFirewallOp(true, "deny"))

	rules := f.chain("KC-HFW")
	if len(rules) == 0 {
		t.Fatalf("链为空，规则根本没写入:\n%s", f.calls())
	}
	const tail = "-j DROP"
	if got := rules[len(rules)-1]; got != tail {
		t.Fatalf("链尾必须是 %q（兜底拒绝），实际 %q；全链：\n%s", tail, got, strings.Join(rules, "\n"))
	}
	if n := countIn(rules, tail); n != 1 {
		t.Errorf("兜底 DROP 应恰好一条，实际 %d 条:\n%s", n, strings.Join(rules, "\n"))
	}
	// 顺序就是语义：兜底必须排在所有放行之后，否则（例如白名单）永远不命中。
	wl := "-s 192.168.64.1 -j ACCEPT"
	idx := -1
	for i, r := range rules {
		if r == wl {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("白名单放行缺失:\n%s", strings.Join(rules, "\n"))
	}
	if idx > len(rules)-2 {
		t.Errorf("兜底 DROP 必须排在白名单放行之后:\n%s", strings.Join(rules, "\n"))
	}
	// 不得越权改宿主机自己的默认策略（那是 ufw 的地盘，且无法随本链回滚）。
	if f.hasCall("-P INPUT") {
		t.Errorf("不应改 INPUT 默认策略（链级兜底已等价且可回滚）:\n%s", f.calls())
	}
	// 但仍要挂进 INPUT，否则整条链没有任何流量经过。
	if !f.hasCall("-I INPUT 1 -j KC-HFW") {
		t.Errorf("启用时必须把链挂进 INPUT:\n%s", f.calls())
	}
}

// TestHostFirewallAcceptHasNoDropFallback 覆盖默认放行：此时不该有兜底 DROP
// （否则「放行」的配置会把未匹配流量全拒掉）。
func TestHostFirewallAcceptHasNoDropFallback(t *testing.T) {
	f := installFakeIptables(t)
	applyHostFirewall(t, hostFirewallOp(true, "accept"))

	rules := f.chain("KC-HFW")
	if n := countIn(rules, "-j DROP"); n != 0 {
		t.Fatalf("默认放行时不该有兜底 DROP，实际 %d 条:\n%s", n, strings.Join(rules, "\n"))
	}
	if len(rules) == 0 {
		t.Fatal("规则仍应写入")
	}
}

// TestHostFirewallDisabledHasNoDropFallback 覆盖启用开关：停用时链会被摘出
// INPUT，此时写入兜底 DROP 只会留下一条"看起来生效"的死规则。
func TestHostFirewallDisabledHasNoDropFallback(t *testing.T) {
	f := installFakeIptables(t)
	applyHostFirewall(t, hostFirewallOp(false, "deny"))

	if n := countIn(f.chain("KC-HFW"), "-j DROP"); n != 0 {
		t.Fatalf("停用时不该写兜底 DROP，实际 %d 条:\n%s", n, strings.Join(f.chain("KC-HFW"), "\n"))
	}
	if f.hasCall("-I INPUT 1 -j KC-HFW") {
		t.Errorf("停用时不得挂进 INPUT:\n%s", f.calls())
	}
}

// TestHostFirewallApplyIsIdempotent 覆盖重复 apply：链每次都被 -F 重建，
// 因此兜底 DROP 不能一条叠一条地长出来。
func TestHostFirewallApplyIsIdempotent(t *testing.T) {
	f := installFakeIptables(t)
	op := hostFirewallOp(true, "deny")
	applyHostFirewall(t, op)
	first := f.chain("KC-HFW")
	applyHostFirewall(t, op)
	second := f.chain("KC-HFW")

	if n := countIn(second, "-j DROP"); n != 1 {
		t.Fatalf("重复 apply 后兜底 DROP 应仍只有一条，实际 %d 条:\n%s", n, strings.Join(second, "\n"))
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Errorf("重复 apply 的链内容应完全一致：\n第一次:\n%s\n第二次:\n%s",
			strings.Join(first, "\n"), strings.Join(second, "\n"))
	}
}
