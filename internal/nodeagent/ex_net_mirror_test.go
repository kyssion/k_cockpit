package nodeagent

// 本文件覆盖端口镜像的 P2 缺陷（实机确认）：镜像不复制任何流量。
//
// 缺陷的形状只在**命令层**可见：任务回报 success，`ovs-vsctl list Mirror`
// 里也有一条行，但那行没有 output_port——OVS 没有复制目标，一个报文都不会
// 被复制；多源、方向、多目标全部被静默忽略（原实现只取 sources[0]）。
//
// 因此这里用可记账的 ovs-vsctl / ip 桩，既记录**每条命令的完整参数**（断言
// 命令形状与顺序），也维护极简拓扑状态（桥、端口、Mirror 行、veth 对），
// 这样才能断言「最终拓扑」——幂等与回收的正确性只体现在拓扑里，不体现在
// 某一条命令里。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/agent"
)

// fakeOVSVsctlScript 是记账版 ovs-vsctl：日志 + 极简状态（bridges/ports/
// mirrors）。状态必须模拟，而不是「只记日志」：
//   - 端口是否存在决定 get Port 能否成功（引用列必须先换成 UUID）；
//   - 摘掉 Bridge.mirrors 的最后一个引用会让 Mirror 行被 GC（真机实测）；
//   - add-port/del-port 决定最终拓扑，只记日志无法区分「重复 enable 叠了
//     两套端口」与「只有一套」。
const fakeOVSVsctlScript = `#!/bin/sh
printf 'ovs-vsctl %s\n' "$*" >> "$KC_MIR_LOG"
all="$*"
st="$KC_MIR_STATE"
if [ -n "$KC_MIR_FAIL" ]; then
  case "$all" in
    *"$KC_MIR_FAIL"*) echo "stub failure" >&2; exit 1 ;;
  esac
fi
case "$all" in
  *"create Mirror"*)
    row=$(printf '%s' "$all" | sed -n 's/.*name=\([^ ]*\).*/\1/p' | head -1)
    out=$(printf '%s' "$all" | sed -n 's/.*--id=@o get Port \([^ ]*\).*/\1/p' | head -1)
    printf '%s %s\n' "$row" "$out" >> "$st/mirrors"
    exit 0
    ;;
  *"remove Bridge"*)
    row=$(printf '%s' "$all" | sed -n 's/.*get Mirror \([^ ]*\) _uuid.*/\1/p' | head -1)
    if [ -n "$row" ] && [ -f "$st/mirrors" ]; then
      grep -v "^$row " "$st/mirrors" > "$st/m.tmp" || :
      mv "$st/m.tmp" "$st/mirrors"
    fi
    exit 0
    ;;
esac
case "$all" in
  *"--columns=name"*)
    [ -f "$st/mirrors" ] && awk '{print $1}' "$st/mirrors"
    exit 0
    ;;
esac
case "$all" in
  "br-exists "*)
    br=${all#br-exists }
    [ -f "$st/bridges" ] && grep -qx -- "$br" "$st/bridges" && exit 0
    exit 1
    ;;
  "iface-to-br "*)
    ifc=${all#iface-to-br }
    br=$(awk -v i="$ifc" '$2==i {print $1; exit}' "$st/ports" 2>/dev/null)
    if [ -n "$br" ]; then printf '%s\n' "$br"; exit 0; fi
    printf 'no bridge\n'
    exit 0
    ;;
  "list-br")
    [ -f "$st/bridges" ] && cat "$st/bridges"
    exit 0
    ;;
  "list-ports "*)
    br=${all#list-ports }
    [ -f "$st/ports" ] && awk -v b="$br" '$1==b {print $2}' "$st/ports"
    exit 0
    ;;
esac
case "$all" in
  *"get Mirror "*" name")
    row=$(printf '%s' "$all" | awk '{print $3}')
    if [ -f "$st/mirrors" ] && grep -q "^$row " "$st/mirrors"; then
      printf '%s\n' "$row"
      exit 0
    fi
    exit 1
    ;;
  *"get Mirror "*" _uuid")
    row=$(printf '%s' "$all" | awk '{print $3}')
    [ -f "$st/mirrors" ] && grep -q "^$row " "$st/mirrors" || exit 1
    printf 'uuid-%s\n' "$row"
    exit 0
    ;;
  *"get Port "*)
    port=$(printf '%s' "$all" | awk '{print $4}')
    [ -f "$st/ports" ] && awk -v p="$port" '$2==p {f=1} END {exit !f}' "$st/ports" || exit 1
    printf 'uuid-%s\n' "$port"
    exit 0
    ;;
esac
case "$all" in
  *"add-port "*)
    pair=$(printf '%s' "$all" | sed -n 's/.*add-port \([^ ]*\) \([^ ]*\).*/\1 \2/p' | head -1)
    br=${pair%% *}
    port=${pair##* }
    if [ -n "$br" ] && [ -n "$port" ]; then
      grep -qx -- "$br $port" "$st/ports" 2>/dev/null || printf '%s %s\n' "$br" "$port" >> "$st/ports"
    fi
    exit 0
    ;;
  *"del-port "*)
    pair=$(printf '%s' "$all" | sed -n 's/.*del-port \([^ ]*\) \([^ ]*\).*/\1 \2/p' | head -1)
    br=${pair%% *}
    port=${pair##* }
    if [ -n "$br" ] && [ -n "$port" ] && [ -f "$st/ports" ]; then
      grep -vx -- "$br $port" "$st/ports" > "$st/p.tmp" || :
      mv "$st/p.tmp" "$st/ports"
    fi
    exit 0
    ;;
esac
exit 0
`

// fakeIPScript 是记账版 ip：维护 veth 对（删一端即删两端，与内核一致）。
const fakeIPScript = `#!/bin/sh
printf 'ip %s\n' "$*" >> "$KC_MIR_LOG"
all="$*"
st="$KC_MIR_STATE"
case "$all" in
  "link add "*)
    a=$(printf '%s' "$all" | awk '{print $3}')
    b=$(printf '%s' "$all" | awk '{print $8}')
    if [ -f "$st/links" ] && awk -v n="$a" '$1==n || $2==n {f=1} END {exit !f}' "$st/links"; then
      printf 'RTNETLINK answers: File exists\n' >&2
      exit 1
    fi
    printf '%s %s\n' "$a" "$b" >> "$st/links"
    exit 0
    ;;
  "link del "*)
    d=${all#link del }
    if [ ! -f "$st/links" ] || ! awk -v n="$d" '$1==n || $2==n {f=1} END {exit !f}' "$st/links"; then
      printf 'Cannot find device "%s"\n' "$d" >&2
      exit 1
    fi
    awk -v n="$d" '$1!=n && $2!=n' "$st/links" > "$st/l.tmp"
    mv "$st/l.tmp" "$st/links"
    exit 0
    ;;
esac
exit 0
`

// fakeMirrorNet 是一次测试里的假 ovs-vsctl + ip（自带拓扑状态与调用日志）。
type fakeMirrorNet struct {
	t       *testing.T
	state   string
	logPath string
}

// installFakeMirrorNet 装好假命令，并把镜像运行态目录指到临时目录。
//
// mirrorRunDir 必须能被改写：/run 在 mac 上不存在，测试里也无权写。
func installFakeMirrorNet(t *testing.T, bridges []string, ports map[string][]string) *fakeMirrorNet {
	t.Helper()
	installFakeCommands(t, map[string]string{
		"ovs-vsctl": fakeOVSVsctlScript,
		"ip":        fakeIPScript,
	})
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatalf("建状态目录失败: %v", err)
	}
	logPath := filepath.Join(dir, "calls.log")
	for name, body := range map[string]string{
		"bridges": strings.Join(bridges, "\n") + "\n",
		"ports":   renderPortTable(ports),
		"mirrors": "",
		"links":   "",
	} {
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0o644); err != nil {
			t.Fatalf("写状态文件 %s 失败: %v", name, err)
		}
	}
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("建调用日志失败: %v", err)
	}
	t.Setenv("KC_MIR_STATE", state)
	t.Setenv("KC_MIR_LOG", logPath)

	old := mirrorRunDir
	mirrorRunDir = filepath.Join(dir, "run")
	if err := os.MkdirAll(mirrorRunDir, 0o755); err != nil {
		t.Fatalf("建运行态目录失败: %v", err)
	}
	t.Cleanup(func() { mirrorRunDir = old })

	return &fakeMirrorNet{t: t, state: state, logPath: logPath}
}

// renderPortTable 把「桥 → 端口」渲染成状态文件的行式布局（"桥 端口"）。
func renderPortTable(ports map[string][]string) string {
	var b strings.Builder
	for br, list := range ports {
		for _, p := range list {
			b.WriteString(br + " " + p + "\n")
		}
	}
	return b.String()
}

func (f *fakeMirrorNet) lines(file string) []string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.state, file))
	if err != nil {
		f.t.Fatalf("读状态文件 %s 失败: %v（调用：\n%s）", file, err, f.calls())
	}
	out := make([]string, 0, 4)
	for _, line := range strings.Split(string(raw), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// calls 返回全部被发出的命令（每条一行，形如 "ovs-vsctl br-exists br-int"）。
func (f *fakeMirrorNet) calls() string {
	f.t.Helper()
	raw, err := os.ReadFile(f.logPath)
	if err != nil {
		return "(读不到调用日志)"
	}
	return string(raw)
}

// mirrorRows 返回当前的 Mirror 行名。
func (f *fakeMirrorNet) mirrorRows() []string {
	f.t.Helper()
	rows := make([]string, 0, 2)
	for _, line := range f.lines("mirrors") {
		rows = append(rows, strings.Fields(line)[0])
	}
	return rows
}

// mirrorOutput 返回某条 Mirror 行的 output_port（没有该行返回空串）。
func (f *fakeMirrorNet) mirrorOutput(row string) string {
	f.t.Helper()
	for _, line := range f.lines("mirrors") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == row {
			return fields[1]
		}
	}
	return ""
}

// portsOf 返回某座桥上的全部端口。
func (f *fakeMirrorNet) portsOf(bridge string) []string {
	f.t.Helper()
	out := make([]string, 0, 4)
	for _, line := range f.lines("ports") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == bridge {
			out = append(out, fields[1])
		}
	}
	return out
}

// allPorts 返回全部「桥 端口」条目。
func (f *fakeMirrorNet) allPorts() []string { f.t.Helper(); return f.lines("ports") }

// links 返回全部 veth 对（"a b"）。
func (f *fakeMirrorNet) links() []string { f.t.Helper(); return f.lines("links") }

// hasCall 报告是否发出过含 needle 的命令。
func (f *fakeMirrorNet) hasCall(needle string) bool {
	f.t.Helper()
	for _, line := range strings.Split(f.calls(), "\n") {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// countCalls 统计命中 needle 的命令条数。
func (f *fakeMirrorNet) countCalls(needle string) int {
	f.t.Helper()
	n := 0
	for _, line := range strings.Split(f.calls(), "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// mirrorCreateCall 返回建立 Mirror 行的那条事务（唯一一条 command 含
// "create Mirror"），失败时给出全部调用。
func (f *fakeMirrorNet) mirrorCreateCall() string {
	f.t.Helper()
	for _, line := range strings.Split(f.calls(), "\n") {
		if strings.Contains(line, "create Mirror") {
			return line
		}
	}
	f.t.Fatalf("没有任何 create Mirror 事务——镜像根本没有建立：\n%s", f.calls())
	return ""
}

// mirrorOp 构造控制面 Enable 下发的真实参数形状（见 portmirror.Service.Enable）。
func mirrorOp(name string, sources, targets []string, direction string, watchdog int) agent.Operation {
	src := make([]any, 0, len(sources))
	for _, s := range sources {
		src = append(src, s)
	}
	dst := make([]any, 0, len(targets))
	for _, s := range targets {
		dst = append(dst, s)
	}
	return agent.Operation{
		Kind:   agent.OpPortMirrorEnable,
		NodeID: 1,
		Target: name,
		Params: map[string]any{
			"name":             name,
			"source_ports":     src,
			"target_switches":  dst,
			"direction":        direction,
			"vlan_preserve":    true,
			"watchdog_seconds": float64(watchdog),
		},
	}
}

// enableMirror 跑一次 enable 并要求成功。
func enableMirror(t *testing.T, op agent.Operation) *agent.PortMirrorInfo {
	t.Helper()
	res, err := portMirrorEnable(context.Background(), op, func(Stage) {})
	if err != nil {
		t.Fatalf("启用镜像返回 error: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("启用镜像应成功，实际 %#v", res)
	}
	info, ok := res.Data[agent.PortMirrorDataKey].(agent.PortMirrorInfo)
	if !ok {
		t.Fatalf("结果形状不对: %#v", res.Data)
	}
	return &info
}

// TestPortMirrorEnableMultiSourceCrossBridge 是缺陷的主症状用例：
// 多源 + 跨桥目标必须建出「veth 对 + 两端 add-port + 带 output_port 的
// Mirror 行」，且两个选择列都覆盖**全部**源端口。
func TestPortMirrorEnableMultiSourceCrossBridge(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0", "vnet1"},
		"br-dst": {},
	})

	info := enableMirror(t, mirrorOp("m1", []string{"vnet0", "vnet1"}, []string{"br-dst"}, "both", -1))
	if info.Applied != 1 {
		t.Errorf("应建立 1 条镜像行，实际 %d", info.Applied)
	}

	base := mirrorPortBase("m1", "br-dst")
	out, peer := base+"a", base+"b"
	if len(out) > 15 || len(peer) > 15 {
		t.Fatalf("端口名 %d/%d 字符，超过 Linux ifname 上限 15", len(out), len(peer))
	}

	// 1) veth 对与两端接入。
	if !f.hasCall("ip link add " + out + " type veth peer name " + peer) {
		t.Errorf("必须建 veth 对 %s/%s：\n%s", out, peer, f.calls())
	}
	if !f.hasCall("add-port br-src " + out) {
		t.Errorf("veth 源桥端必须接入源桥：\n%s", f.calls())
	}
	if !f.hasCall("add-port br-dst " + peer) {
		t.Errorf("veth 目标桥端必须接入目标交换机：\n%s", f.calls())
	}
	if len(f.links()) != 1 {
		t.Errorf("应恰好一对 veth，实际 %v", f.links())
	}
	if got := f.portsOf("br-src"); len(got) != 3 {
		t.Errorf("源桥端口应含 2 个源口 + 1 个输出口，实际 %v", got)
	}

	// 2) Mirror 行：两个选择列都含全部源端口，且 output_port 指向源桥端。
	create := f.mirrorCreateCall()
	if !strings.Contains(create, "select_src_port=@p0,@p1") {
		t.Errorf("select_src_port 必须包含全部源端口（@p0,@p1）：\n%s", create)
	}
	if !strings.Contains(create, "select_dst_port=@p0,@p1") {
		t.Errorf("select_dst_port 必须包含全部源端口（@p0,@p1）：\n%s", create)
	}
	if !strings.Contains(create, "output_port=@o") {
		t.Errorf("**没有 output_port 的镜像不复制任何流量**（本次缺陷）：\n%s", create)
	}
	if !strings.Contains(create, "--id=@p1 get Port vnet1") {
		t.Errorf("第二个源端口也必须换成 UUID 引用（否则多源静默退化）：\n%s", create)
	}
	rows := f.mirrorRows()
	if len(rows) != 1 || rows[0] != "kc-mir-m1-0" {
		t.Fatalf("应只有一条 kc-mir-m1-0，实际 %v", rows)
	}
	if got := f.mirrorOutput("kc-mir-m1-0"); got != out {
		t.Errorf("output_port 应指向源桥上的 %s，实际 %q", out, got)
	}

	// 3) 现场落盘（看门狗与 disable 依赖它精确回收）。
	st, ok := readMirrorState("m1")
	if !ok {
		t.Fatal("必须把现场写入 state 文件")
	}
	if st.Source != "br-src" || len(st.Mirrors) != 1 || len(st.Ports) != 2 || len(st.Veths) != 1 {
		t.Fatalf("state 内容不对: %+v", st)
	}
	fi, err := os.Stat(mirrorStatePath("m1"))
	if err != nil {
		t.Fatalf("state 文件不存在: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("state 文件权限应为 0600，实际 %o", perm)
	}
}

// TestPortMirrorEnableSameBridgeUsesInternalPort 覆盖「目标与源同桥」：
// 必须用 internal 端口，绝不能在源桥上接 veth 两端（那会成环）。
func TestPortMirrorEnableSameBridgeUsesInternalPort(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-int"}, map[string][]string{
		"br-int": {"vnet0"},
	})

	enableMirror(t, mirrorOp("same", []string{"vnet0"}, []string{"br-int"}, "both", -1))

	if f.hasCall("ip link add") {
		t.Errorf("同桥目标不得创建 veth（两端接同一座桥会成环）：\n%s", f.calls())
	}
	if len(f.links()) != 0 {
		t.Errorf("同桥目标不应留下 veth：%v", f.links())
	}
	base := mirrorPortBase("same", "br-int")
	out := base + "a"
	if !f.hasCall("add-port br-int " + out + " -- set Interface " + out + " type=internal") {
		t.Errorf("同桥目标必须建 internal 端口 %s：\n%s", out, f.calls())
	}
	if got := f.mirrorOutput("kc-mir-same-0"); got != out {
		t.Errorf("output_port 应指向 internal 端口 %s，实际 %q", out, got)
	}
	st, ok := readMirrorState("same")
	if !ok || len(st.Ports) != 1 || st.Ports[0].Kind != mirrorKindInternal || len(st.Veths) != 0 {
		t.Fatalf("state 应只记录一个 internal 端口、零个 veth: %+v", st)
	}
}

// TestPortMirrorEnableDirection 覆盖方向映射：ingress 只设 src、egress 只设
// dst、both 两者都设；任何方向的选择列都必须含全部源端口。
func TestPortMirrorEnableDirection(t *testing.T) {
	cases := []struct {
		direction string
		wantSrc   bool
		wantDst   bool
	}{
		{"both", true, true},
		{"ingress", true, false},
		{"egress", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.direction, func(t *testing.T) {
			f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
				"br-src": {"vnet0", "vnet1"},
				"br-dst": {},
			})
			enableMirror(t, mirrorOp("dir", []string{"vnet0", "vnet1"}, []string{"br-dst"}, tc.direction, -1))

			create := f.mirrorCreateCall()
			if got := strings.Contains(create, "select_src_port="); got != tc.wantSrc {
				t.Errorf("方向 %s：select_src_port 出现=%v，期望 %v\n%s", tc.direction, got, tc.wantSrc, create)
			}
			if got := strings.Contains(create, "select_dst_port="); got != tc.wantDst {
				t.Errorf("方向 %s：select_dst_port 出现=%v，期望 %v\n%s", tc.direction, got, tc.wantDst, create)
			}
			// 方向不能削减源端口集合。
			if tc.wantSrc && !strings.Contains(create, "select_src_port=@p0,@p1") {
				t.Errorf("方向 %s：src 选择列必须含全部源端口\n%s", tc.direction, create)
			}
			if tc.wantDst && !strings.Contains(create, "select_dst_port=@p0,@p1") {
				t.Errorf("方向 %s：dst 选择列必须含全部源端口\n%s", tc.direction, create)
			}
		})
	}
}

// TestPortMirrorEnableMultiTarget 覆盖多目标：每个目标一条 Mirror 行
// （一个 Mirror 只能有一个 output_port），互不覆盖；跨桥目标用 veth、
// 同桥目标用 internal 端口，两种通路并存。
func TestPortMirrorEnableMultiTarget(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst", "br-dst2"}, map[string][]string{
		"br-src":  {"vnet0"},
		"br-dst":  {},
		"br-dst2": {},
	})
	info := enableMirror(t, mirrorOp("multi", []string{"vnet0"},
		[]string{"br-dst", "br-src"}, "both", -1))
	if info.Applied != 2 {
		t.Errorf("两个目标应建立两条镜像行，实际 %d", info.Applied)
	}
	rows := f.mirrorRows()
	if len(rows) != 2 {
		t.Fatalf("应有两条镜像行，实际 %v", rows)
	}
	cross := mirrorPortBase("multi", "br-dst")
	same := mirrorPortBase("multi", "br-src")
	if got := f.mirrorOutput("kc-mir-multi-0"); got != cross+"a" {
		t.Errorf("第 0 条行（跨桥目标）output_port 应为 %s，实际 %q", cross+"a", got)
	}
	if got := f.mirrorOutput("kc-mir-multi-1"); got != same+"a" {
		t.Errorf("第 1 条行（同桥目标）output_port 应为 %s，实际 %q", same+"a", got)
	}
	if len(f.links()) != 1 {
		t.Errorf("只有跨桥目标需要 veth，实际 %v", f.links())
	}
	st, ok := readMirrorState("multi")
	if !ok || len(st.Mirrors) != 2 || len(st.Ports) != 3 || len(st.Veths) != 1 {
		t.Fatalf("state 应记录 2 行 / 3 个端口 / 1 对 veth: %+v", st)
	}
}

// TestPortMirrorEnableIsIdempotent 覆盖重复 enable：先清旧行与旧端口，
// 最终只留一套拓扑（一条 Mirror 行、一套端口、一对 veth）。
func TestPortMirrorEnableIsIdempotent(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0"},
		"br-dst": {},
	})
	op := mirrorOp("again", []string{"vnet0"}, []string{"br-dst"}, "both", -1)

	enableMirror(t, op)
	firstPorts := strings.Join(f.allPorts(), ",")
	firstLinks := strings.Join(f.links(), ",")
	enableMirror(t, op)

	if rows := f.mirrorRows(); len(rows) != 1 {
		t.Fatalf("重复 enable 后应只有一条镜像行，实际 %v", rows)
	}
	if got := strings.Join(f.allPorts(), ","); got != firstPorts {
		t.Errorf("重复 enable 后端口集合应保持一致：\n第一次 %s\n第二次 %s", firstPorts, got)
	}
	if got := strings.Join(f.links(), ","); got != firstLinks {
		t.Errorf("重复 enable 后 veth 应保持一致（不留旧 veth）：\n第一次 %s\n第二次 %s", firstLinks, got)
	}
	// 重建必须真的先摘行，而不是又 create 一条同名行。
	if n := f.countCalls("remove Bridge"); n == 0 {
		t.Errorf("重复 enable 必须先摘掉同名旧镜像行：\n%s", f.calls())
	}
	if n := f.countCalls("ip link del"); n == 0 {
		t.Errorf("重复 enable 必须回收旧 veth：\n%s", f.calls())
	}
}

// TestPortMirrorDisableCleansEverything 覆盖 disable：行被摘、端口被 del、
// veth 被删、运行态文件清空。
func TestPortMirrorDisableCleansEverything(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0", "vnet1"},
		"br-dst": {},
	})
	enableMirror(t, mirrorOp("off", []string{"vnet0", "vnet1"}, []string{"br-dst"}, "both", -1))

	res, err := portMirrorDisable(context.Background(), agent.Operation{
		Kind: agent.OpPortMirrorDisable, NodeID: 1, Target: "off",
	}, func(Stage) {})
	if err != nil || res == nil || !res.Success {
		t.Fatalf("撤销应成功: res=%#v err=%v", res, err)
	}

	base := mirrorPortBase("off", "br-dst")
	if rows := f.mirrorRows(); len(rows) != 0 {
		t.Errorf("撤销后不应残留镜像行，实际 %v", rows)
	}
	if ports := f.allPorts(); len(ports) != 2 {
		// 只剩两个源端口（vnet0/vnet1）。
		t.Errorf("撤销后桥上应只剩源端口，实际 %v", ports)
	}
	if !f.hasCall("del-port br-src "+base+"a") || !f.hasCall("del-port br-dst "+base+"b") {
		t.Errorf("撤销必须把两端镜像端口从桥上摘掉：\n%s", f.calls())
	}
	if !f.hasCall("ip link del " + base + "a") {
		t.Errorf("撤销必须删除 veth：\n%s", f.calls())
	}
	if links := f.links(); len(links) != 0 {
		t.Errorf("撤销后不应残留 veth，实际 %v", links)
	}
	for _, p := range []string{mirrorStatePath("off"), mirrorConfirmPath("off"), mirrorWDFile("off")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("撤销后运行态文件应被删除: %s", p)
		}
	}
}

// TestPortMirrorEnableRejectsMissingTarget 覆盖「目标交换机不存在」：
// 明确失败，且**不创建任何 veth/端口/Mirror 行**（不留半套拓扑）。
func TestPortMirrorEnableRejectsMissingTarget(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src"}, map[string][]string{
		"br-src": {"vnet0"},
	})

	res, err := portMirrorEnable(context.Background(),
		mirrorOp("bad", []string{"vnet0"}, []string{"br-missing"}, "both", -1), func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error（失败走 Result）: %v", err)
	}
	if res.Success {
		t.Fatal("目标交换机不存在时必须失败")
	}
	if !strings.Contains(res.Message, "br-missing") {
		t.Errorf("错误文案必须点名目标交换机，实际: %s", res.Message)
	}
	if f.hasCall("ip link add") || f.hasCall("add-port") || f.hasCall("create Mirror") {
		t.Errorf("校验失败时不得创建任何东西：\n%s", f.calls())
	}
	if _, ok := readMirrorState("bad"); ok {
		t.Error("校验失败时不应写 state 文件")
	}
}

// TestPortMirrorEnableRejectsCrossBridgeSources 覆盖跨桥源：
// 一个 Mirror 只能挂一座桥，跨桥必须明确失败（而不是只镜像第一个口）。
func TestPortMirrorEnableRejectsCrossBridgeSources(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-a", "br-b", "br-dst"}, map[string][]string{
		"br-a":   {"vnet0"},
		"br-b":   {"vnet1"},
		"br-dst": {},
	})

	res, err := portMirrorEnable(context.Background(),
		mirrorOp("mix", []string{"vnet0", "vnet1"}, []string{"br-dst"}, "both", -1), func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("跨交换机源必须明确失败")
	}
	if !strings.Contains(res.Message, "跨交换机") {
		t.Errorf("错误文案应说明跨交换机源不支持，实际: %s", res.Message)
	}
	if f.hasCall("ip link add") || f.hasCall("create Mirror") {
		t.Errorf("失败时不得建任何拓扑：\n%s", f.calls())
	}
}

// TestPortMirrorEnableRollsBackOnMirrorFailure 覆盖「建行失败要回滚」：
// 已经建好的 veth 与端口必须全部回收，不能在节点上留半套拓扑。
func TestPortMirrorEnableRollsBackOnMirrorFailure(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0"},
		"br-dst": {},
	})
	// 只让 create Mirror 失败（veth 与端口已经建好）。
	t.Setenv("KC_MIR_FAIL", "create Mirror")

	res, err := portMirrorEnable(context.Background(),
		mirrorOp("boom", []string{"vnet0"}, []string{"br-dst"}, "both", -1), func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("create Mirror 失败时启用必须失败")
	}
	if !strings.Contains(res.Message, "回滚") {
		t.Errorf("错误文案应说明已回滚，实际: %s", res.Message)
	}
	if rows := f.mirrorRows(); len(rows) != 0 {
		t.Errorf("失败后不应残留镜像行: %v", rows)
	}
	if ports := f.allPorts(); len(ports) != 1 {
		t.Errorf("失败后桥上应只剩源端口，实际 %v", ports)
	}
	if links := f.links(); len(links) != 0 {
		t.Errorf("失败后不应残留 veth，实际 %v", links)
	}
	if _, ok := readMirrorState("boom"); ok {
		t.Error("失败时不应写 state 文件")
	}
}

// TestPortMirrorRearmDropsExpiredMirror 覆盖 agent 重启后的补撤销：
// 看门狗文件写着已过期的时刻、且没有 confirm 文件 → 镜像必须被整套撤掉。
func TestPortMirrorRearmDropsExpiredMirror(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0"},
		"br-dst": {},
	})
	enableMirror(t, mirrorOp("wd", []string{"vnet0"}, []string{"br-dst"}, "both", -1))

	body := fmt.Sprintf("%d\n%s", timeNowSec()-5, "wd")
	if err := osWriteFile(mirrorWDFile("wd"), []byte(body), 0o600); err != nil {
		t.Fatalf("写看门狗文件失败: %v", err)
	}
	rearmMirrorWatchdogs()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.mirrorRows()) == 0 && len(f.links()) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("过期看门狗必须撤销镜像：rows=%v links=%v\n%s",
		f.mirrorRows(), f.links(), f.calls())
}

// TestPortMirrorConfirmKeepsMirror 覆盖确认语义：写 confirm 文件并清掉
// 看门狗文件，之后的到期撤销不得拆掉镜像。
func TestPortMirrorConfirmKeepsMirror(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0"},
		"br-dst": {},
	})
	enableMirror(t, mirrorOp("keep", []string{"vnet0"}, []string{"br-dst"}, "both", 60))

	if err := osWriteFile(mirrorWDFile("keep"), []byte("1\nkeep"), 0o600); err != nil {
		t.Fatalf("写看门狗文件失败: %v", err)
	}
	if _, err := portMirrorConfirm(context.Background(), agent.Operation{
		Kind: agent.OpPortMirrorConfirm, NodeID: 1, Target: "keep",
	}, func(Stage) {}); err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if _, err := osStat(mirrorWDFile("keep")); err == nil {
		t.Fatal("确认后必须清掉看门狗文件（否则重启会重臂）")
	}
	fireMirrorWatchdog("keep")
	if rows := f.mirrorRows(); len(rows) != 1 {
		t.Errorf("已确认的镜像不得被撤销，实际行: %v", rows)
	}
}

// TestPortMirrorEnableRecoversFromStaleVeth 覆盖「上次留下的同名 veth」：
// agent 在落盘前被杀会留下设备而没有状态文件，此时 enable 必须能自愈
// （端口名由名字哈希而来，同名设备只可能是本镜像自己的残留）。
func TestPortMirrorEnableRecoversFromStaleVeth(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0"},
		"br-dst": {},
	})
	base := mirrorPortBase("stale", "br-dst")
	// 预置一对残留 veth（模拟「建了设备但状态没落盘」）。
	if err := osWriteFile(filepath.Join(f.state, "links"),
		[]byte(base+"a "+base+"b\n"), 0o644); err != nil {
		t.Fatalf("预置残留 veth 失败: %v", err)
	}

	enableMirror(t, mirrorOp("stale", []string{"vnet0"}, []string{"br-dst"}, "both", -1))

	if links := f.links(); len(links) != 1 || links[0] != base+"a "+base+"b" {
		t.Fatalf("应只剩一对（自己的）veth，实际 %v", links)
	}
	if rows := f.mirrorRows(); len(rows) != 1 {
		t.Fatalf("应有一条镜像行，实际 %v", rows)
	}
}

// TestPortMirrorRepairKeepsWatchdogWindow 覆盖修复重下发（watchdog_seconds
// 为负）：拓扑要重建，但原来的自动撤销窗口必须保留——否则一次修复就把
// 「N 分钟后自动撤销」变成永久镜像。
func TestPortMirrorRepairKeepsWatchdogWindow(t *testing.T) {
	f := installFakeMirrorNet(t, []string{"br-src", "br-dst"}, map[string][]string{
		"br-src": {"vnet0"},
		"br-dst": {},
	})
	enableMirror(t, mirrorOp("repair", []string{"vnet0"}, []string{"br-dst"}, "both", -1))

	deadline := timeNowSec() + 600
	if err := osWriteFile(mirrorWDFile("repair"),
		[]byte(fmt.Sprintf("%d\nrepair", deadline)), 0o600); err != nil {
		t.Fatalf("写看门狗文件失败: %v", err)
	}
	// 修复重下发：watchdog_seconds = -1。
	enableMirror(t, mirrorOp("repair", []string{"vnet0"}, []string{"br-dst"}, "both", -1))

	if rows := f.mirrorRows(); len(rows) != 1 {
		t.Fatalf("修复后应仍只有一条镜像行，实际 %v", rows)
	}
	got, ok := readMirrorWatchdog("repair")
	if !ok {
		t.Fatal("修复重下发不得丢掉原有的自动撤销窗口")
	}
	if got != deadline {
		t.Errorf("到期时刻应保持 %d（不重新计时），实际 %d", deadline, got)
	}
}

// TestMirrorRowNameMatchingIsPrecise 覆盖前缀匹配的边界：叫 web 的镜像
// 不得把叫 web-2 的镜像的行当成自己的（删别人的镜像比残留更糟）。
func TestMirrorRowNameMatchingIsPrecise(t *testing.T) {
	cases := []struct {
		row   string
		name  string
		match bool
	}{
		{"kc-mir-web", "web", true},
		{"kc-mir-web-0", "web", true},
		{"kc-mir-web-12", "web", true},
		{"kc-mir-web-2-0", "web", false},
		{"kc-mir-webby-0", "web", false},
		{"kc-mir-web-", "web", false},
	}
	for _, tc := range cases {
		t.Run(tc.row, func(t *testing.T) {
			got := mirrorRowMatches(tc.name, tc.row)
			if got != tc.match {
				t.Fatalf("mirrorRowMatches(%q, %q) = %v，期望 %v", tc.name, tc.row, got, tc.match)
			}
		})
	}
}
