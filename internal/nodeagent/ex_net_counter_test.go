package nodeagent

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// 本文件覆盖第 3 轮 NET-06：`network.counter.reset` 用了
// `ovs-vsctl --all clear Interface statistics`，而 `clear` 的语法是
// `clear table record column...`，**没有 `--all` 选项**——命令必然报错，
// 接口每次 422，这个功能从来没成功过一次。

// TestResetOVSInterfaceCountersPerInterface 覆盖逐条重置。
func TestResetOVSInterfaceCountersPerInterface(t *testing.T) {
	f := &fakeRunner{listOut: "ovsbr0\nvnet0\n"}
	reset, err := resetOVSInterfaceCounters(context.Background(), f.run)
	if err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if reset != 2 {
		t.Errorf("Reset = %d，期望 2", reset)
	}
	if f.find("ovs-vsctl", "clear", "Interface", "ovsbr0", "statistics") < 0 {
		t.Errorf("未重置 ovsbr0:\n%s", f.dump())
	}
	if f.find("ovs-vsctl", "clear", "Interface", "vnet0", "statistics") < 0 {
		t.Errorf("未重置 vnet0:\n%s", f.dump())
	}
	if n := f.countWith("clear"); n != 2 {
		t.Errorf("clear 次数 = %d，期望 2（每个接口一次）", n)
	}
	// 这一条是本缺陷的根因，单独钉住：命令里绝不能出现 --all。
	if f.countWith("--all") != 0 {
		t.Errorf("clear 没有 --all 选项，命令里不得出现:\n%s", f.dump())
	}
	// ovs-vsctl 也没有 `csv,noheader` 这种写法（真机验证：unknown output format）。
	// 首版修复踩过这个坑，这里把正确写法钉死。
	if f.find("ovs-vsctl", "--columns=name", "--no-headings", "--format=csv",
		"list", "Interface") < 0 {
		t.Errorf("列接口的命令写法不对（应为 --columns=name --no-headings --format=csv）:\n%s", f.dump())
	}
	for _, call := range f.calls {
		for _, a := range call {
			if strings.Contains(a, "noheader") {
				t.Errorf("ovs-vsctl 不支持 %q:\n%s", a, f.dump())
			}
		}
	}
}

// TestResetOVSInterfaceCountersEmptyIsSuccess 覆盖空列表。
//
// 宿主机上还没建过任何 OVS 接口时，"没有需要重置的东西"是正常状态。
// 报失败会让用户以为节点坏了。
func TestResetOVSInterfaceCountersEmptyIsSuccess(t *testing.T) {
	for _, out := range []string{"", "\n", "  \n\n"} {
		f := &fakeRunner{listOut: out}
		reset, err := resetOVSInterfaceCounters(context.Background(), f.run)
		if err != nil {
			t.Fatalf("空列表不该失败（out=%q）: %v", out, err)
		}
		if reset != 0 {
			t.Errorf("空列表时 Reset 应为 0，实际 %d", reset)
		}
		if f.countWith("clear") != 0 {
			t.Errorf("空列表时不该发 clear:\n%s", f.dump())
		}
	}
}

// TestResetOVSInterfaceCountersFailureNamesInterface 覆盖失败文案。
//
// 不知道是哪个接口失败时，排查只能靠猜——因此错误里必须带名字。
func TestResetOVSInterfaceCountersFailureNamesInterface(t *testing.T) {
	f := &fakeRunner{listOut: "ovsbr0\nvnet0\n", failName: "ovs-vsctl", failArg: "vnet0"}
	reset, err := resetOVSInterfaceCounters(context.Background(), f.run)
	if err == nil {
		t.Fatal("clear 失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "vnet0") {
		t.Errorf("错误应带失败的接口名，实际: %v", err)
	}
	if reset != 1 {
		t.Errorf("失败前已成功的条数 = %d，期望 1", reset)
	}
}

// TestParseOVSInterfaceNames 覆盖 CSV 解析（含引号与空行）。
func TestParseOVSInterfaceNames(t *testing.T) {
	// 含表头（老版本 / 不认 --no-headings 时的情况）与引号、空行。
	got := parseOVSInterfaceNames("name\n\"ovsbr0\"\nvnet0\n\n  vnet1  \n")
	want := []string{"ovsbr0", "vnet0", "vnet1"}
	if len(got) != len(want) {
		t.Fatalf("解析结果 %#v，期望 %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestCounterResetResultMessages 覆盖 0 与 >0 两种结果的文案区分。
func TestCounterResetResultMessages(t *testing.T) {
	zero := counterResetResult(0)
	info, ok := zero.Data[agent.CounterResetDataKey].(agent.CounterResetInfo)
	if !ok {
		t.Fatalf("结果类型不对: %#v", zero.Data)
	}
	if !zero.Success || info.Reset != 0 {
		t.Errorf("空结果应为 success、Reset=0，实际 %+v", zero)
	}
	if !strings.Contains(info.Message, "没有可重置") {
		t.Errorf("空列表的说明应说明「没有可重置的」，实际: %q", info.Message)
	}

	two := counterResetResult(2)
	info2, ok := two.Data[agent.CounterResetDataKey].(agent.CounterResetInfo)
	if !ok {
		t.Fatalf("结果类型不对: %#v", two.Data)
	}
	if info2.Reset != 2 {
		t.Errorf("Reset = %d，期望 2", info2.Reset)
	}
	if info2.Message == "" || strings.Contains(info2.Message, "没有可重置") {
		t.Errorf("有重置时文案不该说「没有可重置的」，实际: %q", info2.Message)
	}
}
