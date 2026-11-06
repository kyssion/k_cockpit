package nodeagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// 本文件覆盖第 3 轮 DEF-R3-14：`vpc.switch.change` 读出了 `uplink_if` 却
// 从未使用它——交换机建出来没有上行口，节点侧看起来"任务成功"，而实际上
// 上下行通路根本不存在（真机：`ovs-vsctl list-ports` 为空）。
//
// 用可替换的 runner 断言**发出了哪些命令**：这类缺陷（命令写错、字段没被
// 使用、失败没回滚）只能在命令层面钉住，跑一遍真机最多说明"这次通了"。

// fakeRunner 记录被发出的命令，并按需让某条命令失败。
type fakeRunner struct {
	calls [][]string
	// failName/failArg 同时匹配时该条命令返回错误。
	failName string
	failArg  string
	// brExists 为 true 时 `ovs-vsctl br-exists` 成功（桥本来就在）。
	brExists bool
	// listOut 是 `ovs-vsctl list Interface` 的输出。
	listOut string
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))

	if name == "ovs-vsctl" && len(args) == 2 && args[0] == "br-exists" {
		if f.brExists {
			return "", nil
		}
		return "", errors.New("exit status 1") // 真实的 br-exists 在桥不存在时非零
	}
	if name == "ovs-vsctl" && len(args) >= 4 && args[0] == "--columns=name" {
		return f.listOut, nil
	}
	if f.failName != "" && name == f.failName {
		for _, a := range args {
			if a == f.failArg {
				return "", errors.New("命令失败: " + name + " " + strings.Join(args, " "))
			}
		}
	}
	return "", nil
}

// find 返回第一条完整匹配可执行名与全部参数的调用下标；没有返回 -1。
func (f *fakeRunner) find(name string, args ...string) int {
	for i, call := range f.calls {
		if len(call) != len(args)+1 || call[0] != name {
			continue
		}
		match := true
		for j, a := range args {
			if call[j+1] != a {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// countWith 统计参数里含某个词的命令条数（用于断言"没有用错写法"）。
func (f *fakeRunner) countWith(word string) int {
	n := 0
	for _, call := range f.calls {
		for _, a := range call {
			if a == word {
				n++
				break
			}
		}
	}
	return n
}

func (f *fakeRunner) dump() string {
	var b strings.Builder
	for _, call := range f.calls {
		b.WriteString(strings.Join(call, " "))
		b.WriteString("\n")
	}
	return b.String()
}

// TestCreateSwitchBridgeWithoutUplink 覆盖无上行口的交换机（隔离/仅 NAT）。
//
// 没有 uplink_if 是合法配置，不能因此失败，也不能凭空 add-port。
func TestCreateSwitchBridgeWithoutUplink(t *testing.T) {
	f := &fakeRunner{}
	created, err := createSwitchBridge(context.Background(), f.run, "kbr0", switchSpec{Name: "isolated"})
	if err != nil {
		t.Fatalf("无上行口不应失败: %v", err)
	}
	if !created {
		t.Error("桥本来不存在时应回报 created=true")
	}
	if f.countWith("add-port") != 0 {
		t.Errorf("没有上行口时不该 add-port:\n%s", f.dump())
	}
	if f.find("ovs-vsctl", "--may-exist", "add-br", "kbr0") < 0 {
		t.Errorf("应建桥:\n%s", f.dump())
	}
}

// TestCreateSwitchBridgeAttachesUplink 是 DEF-R3-14 的主症状：上行口必须真的接进桥。
func TestCreateSwitchBridgeAttachesUplink(t *testing.T) {
	f := &fakeRunner{}
	const uplink = "kc-r3-upl-3108"

	if _, err := createSwitchBridge(context.Background(), f.run, "kbr7", switchSpec{
		Name: "with-uplink", UplinkIf: uplink,
	}); err != nil {
		t.Fatalf("接上行口不应失败: %v", err)
	}

	addPort := f.find("ovs-vsctl", "--may-exist", "add-port", "kbr7", uplink)
	if addPort < 0 {
		t.Fatalf("必须 ovs-vsctl --may-exist add-port kbr7 %s:\n%s", uplink, f.dump())
	}
	linkUp := f.find("ip", "link", "set", uplink, "up")
	if linkUp < 0 {
		t.Fatalf("接入后必须把上行口拉起:\n%s", f.dump())
	}
	if linkUp < addPort {
		t.Error("必须先 add-port 再 link up")
	}
	if n := f.countWith("add-port"); n != 1 {
		t.Errorf("add-port 次数 = %d，期望 1", n)
	}
	if n := f.countWith("up"); n != 1 {
		t.Errorf("link up 次数 = %d，期望 1（不能重复拉起）", n)
	}

	// OVS 桥**不能**用 Linux 桥的做法：`ip link set <if> master <br>` 对 OVS
	// 不生效（甚至可能成功返回而端口没被 OVS 管起来）。
	if f.countWith("master") != 0 {
		t.Errorf("不得使用 ip link set ... master（那是 Linux 桥的做法）:\n%s", f.dump())
	}
}

// TestCreateSwitchBridgeIsIdempotent 覆盖重复 create（重配置）：
// `--may-exist` 是幂等的关键，桥已存在时应回报 created=false。
func TestCreateSwitchBridgeIsIdempotent(t *testing.T) {
	f := &fakeRunner{brExists: true}
	created, err := createSwitchBridge(context.Background(), f.run, "kbr7", switchSpec{
		Name: "again", UplinkIf: "upl0",
	})
	if err != nil {
		t.Fatalf("重复 create 不应失败: %v", err)
	}
	if created {
		t.Error("桥已存在时必须回报 created=false——控制面据此禁止回滚别人的桥")
	}
	if f.countWith("--may-exist") < 2 {
		t.Errorf("add-br 与 add-port 都应带 --may-exist:\n%s", f.dump())
	}
}

// TestCreateSwitchBridgeRollsBackOnUplinkFailure 覆盖"失败不留空桥"。
//
// 真机上失败后残留的 kbr056539e3 就是这类：任务 failed，宿主机上却多了一座
// 谁也不认识的桥。回滚只删桥，**不碰上行口设备本身**。
func TestCreateSwitchBridgeRollsBackOnUplinkFailure(t *testing.T) {
	f := &fakeRunner{failName: "ovs-vsctl", failArg: "add-port"}
	_, err := createSwitchBridge(context.Background(), f.run, "kbr9", switchSpec{
		Name: "broken", UplinkIf: "upl0",
	})
	if err == nil {
		t.Fatal("add-port 失败时创建必须失败")
	}
	if !strings.Contains(err.Error(), "上行口接入失败") {
		t.Errorf("错误应说明是上行口接入失败，实际: %v", err)
	}
	if f.find("ovs-vsctl", "--if-exists", "del-br", "kbr9") < 0 {
		t.Errorf("失败后必须回滚刚建的桥:\n%s", f.dump())
	}
	if f.find("ip", "link", "del", "kbr9") < 0 {
		t.Errorf("回滚应同时删掉 internal 口:\n%s", f.dump())
	}
	// 回滚不得动上行口：那块网卡不属于我们建的东西。
	if f.find("ip", "link", "del", "upl0") >= 0 || f.find("ip", "link", "set", "upl0", "down") >= 0 {
		t.Errorf("回滚不得删除/关闭上行口设备:\n%s", f.dump())
	}
}

// TestCreateSwitchBridgeKeepsExistingBridgeOnFailure 覆盖"重配置不得拆掉在用的桥"。
//
// `--may-exist add-br` 对已存在的桥也返回成功，此时 created=false。若上行口
// 接入失败就整座桥拆掉，一次失败的重配置会切断桥上所有虚拟机的网络——
// 那是用一次失败的改动换来一次断网。因此这种情况只报错，不拆除。
func TestCreateSwitchBridgeKeepsExistingBridgeOnFailure(t *testing.T) {
	f := &fakeRunner{brExists: true, failName: "ovs-vsctl", failArg: "add-port"}
	created, err := createSwitchBridge(context.Background(), f.run, "kbrLive", switchSpec{
		Name: "live", UplinkIf: "upl0", CIDR: "10.9.0.0/24",
	})
	if err == nil {
		t.Fatal("add-port 失败时创建/重配置必须失败")
	}
	if created {
		t.Error("桥已存在时应回报 created=false")
	}
	if !strings.Contains(err.Error(), "未做拆除") {
		t.Errorf("错误文案应说明未拆除既有网桥，实际: %v", err)
	}
	if f.countWith("del-br") != 0 {
		t.Errorf("既有网桥不得被拆除:\n%s", f.dump())
	}
	if f.find("ip", "link", "del", "kbrLive") >= 0 {
		t.Errorf("既有网桥的 internal 口不得被删除:\n%s", f.dump())
	}
}

// TestDetachSwitchBridgeOrder 覆盖删除顺序：先摘口，再删桥。
func TestDetachSwitchBridgeOrder(t *testing.T) {
	f := &fakeRunner{}
	if err := detachSwitchBridge(context.Background(), f.run, "kbr7", "upl0"); err != nil {
		t.Fatalf("拆除失败: %v", err)
	}
	delPort := f.find("ovs-vsctl", "--if-exists", "del-port", "kbr7", "upl0")
	delBr := f.find("ovs-vsctl", "--if-exists", "del-br", "kbr7")
	if delPort < 0 {
		t.Fatalf("有上行口时必须显式摘口:\n%s", f.dump())
	}
	if delBr < 0 || delBr < delPort {
		t.Errorf("摘口必须发生在删桥之前:\n%s", f.dump())
	}
}

// TestDetachSwitchBridgeSkipsDELPortWhenNoUplink 覆盖无上行口的交换机。
func TestDetachSwitchBridgeSkipsDELPortWhenNoUplink(t *testing.T) {
	f := &fakeRunner{}
	if err := detachSwitchBridge(context.Background(), f.run, "kbr7", ""); err != nil {
		t.Fatalf("拆除失败: %v", err)
	}
	if f.countWith("del-port") != 0 {
		t.Errorf("没有上行口时不该发 del-port:\n%s", f.dump())
	}
	if f.find("ovs-vsctl", "--if-exists", "del-br", "kbr7") < 0 {
		t.Errorf("仍然要删桥:\n%s", f.dump())
	}
}

// TestUplinkBlockedReason 覆盖"别把管理口接进 OVS 桥"。
//
// 把承载默认路由的网卡 add-port 进 OVS 的那一刻，宿主在该网卡上的地址失效，
// 控制面与节点的连接当场断掉——那是比"没有上行口"严重得多的故障。
func TestUplinkBlockedReason(t *testing.T) {
	cases := []struct {
		name     string
		uplink   string
		defIface string
		blocked  bool
	}{
		{"无上行口", "", "eth0", false},
		{"取不到默认路由网卡", "eth9", "", false},
		{"上行口是管理口", "eth0", "eth0", true},
		{"上行口是另一块网卡", "eth9", "eth0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := uplinkBlockedReason(tc.uplink, tc.defIface)
			if tc.blocked && reason == "" {
				t.Fatal("应拒绝把管理口接进 OVS 桥")
			}
			if !tc.blocked && reason != "" {
				t.Fatalf("不该拒绝，实际: %s", reason)
			}
			if tc.blocked && !strings.Contains(reason, tc.uplink) {
				t.Errorf("文案要点名是哪块网卡，实际: %s", reason)
			}
		})
	}
}

// TestSwitchBridgeCreatedKeyDecoded 确认控制面能读到节点回报的 created 标志。
//
// 它决定写记录失败后是否回滚网桥：读不到就一律不回滚（宁可留桥也不误删），
// 所以这个键必须在两侧同名同类型。
func TestSwitchBridgeCreatedKeyDecoded(t *testing.T) {
	if agent.SwitchBridgeCreatedKey == "" {
		t.Fatal("键名不能为空")
	}
	// 节点侧放进 Result.Data 的值必须是 bool，控制面才能断言成 bool。
	res := &agent.Result{Success: true, Data: map[string]any{
		agent.SwitchBridgeCreatedKey: true,
	}}
	if v, ok := res.Data[agent.SwitchBridgeCreatedKey].(bool); !ok || !v {
		t.Fatalf("created 标志应为 true: %#v", res.Data)
	}
}
