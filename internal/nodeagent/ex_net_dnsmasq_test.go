package nodeagent

import (
	"errors"
	"strings"
	"testing"
)

// 本文件覆盖第 3 轮 DEF-R3-15：多台 NAT 交换机时第二台的 dnsmasq 抢
// 127.0.0.1:53，启动即退，而任务仍报 success（面板 active、DHCP 实际不可用）。
//
// 两处根因各有一组用例：
//   - conf 没关 DNS（缺 port=0）→ 断言配置内容；
//   - Start() 成功就当成成功（假 success）→ 用 fake runner 模拟"起得来但立刻死"。

// fakeDnsmasq 模拟进程启动、存活与退出，不需要真的起进程。
type fakeDnsmasq struct {
	nextPID int
	// alive 决定 Start 出来的进程是否"活着"；false 表示启动后立刻退出。
	alive bool
	// exitErr 是已退出进程的错误（如端口占用）。
	exitErr error
	// started 记录每次启动用的 conf 路径，pid → confPath。
	started map[int]string
	killed  []int
}

func newFakeDnsmasq(alive bool) *fakeDnsmasq {
	return &fakeDnsmasq{nextPID: 1000, alive: alive, started: map[int]string{}}
}

func (f *fakeDnsmasq) Start(confPath string) (int, error) {
	f.nextPID++
	f.started[f.nextPID] = confPath
	return f.nextPID, nil
}

func (f *fakeDnsmasq) Alive(int) bool { return f.alive }

func (f *fakeDnsmasq) ExitErr(int) error {
	if f.alive {
		return nil
	}
	if f.exitErr != nil {
		return f.exitErr
	}
	return errors.New("exit status 1")
}

func (f *fakeDnsmasq) Kill(pid int) { f.killed = append(f.killed, pid) }

// TestRenderDnsmasqConfDisablesDNS 是本缺陷的第一处根因：conf 里必须有 port=0。
//
// 没有它，dnsmasq 默认监听 127.0.0.1:53；第二台交换机的实例必然抢不到端口，
// 启动即退（真机：`failed to create listening socket for 127.0.0.1: Address
// already in use`）。这些实例只做 DHCP，本来就不该碰 DNS。
func TestRenderDnsmasqConfDisablesDNS(t *testing.T) {
	conf := renderDnsmasqConf("kbr7", "192.168.99.100", "192.168.99.200", nil)

	if !strings.Contains(conf, "\nport=0\n") {
		t.Fatalf("conf 必须含 port=0（关闭 DNS），实际:\n%s", conf)
	}
	// 不允许出现任何监听 53 / 指定 DNS 端口的写法。
	for _, bad := range []string{"port=53", "listen-address=", "address=/", "server="} {
		if strings.Contains(conf, bad) {
			t.Errorf("conf 不应包含 %q（会重新打开 DNS）:\n%s", bad, conf)
		}
	}
	// 既有行保持不变：改这个配置不能顺手动别的东西。
	for _, want := range []string{
		"interface=kbr7\n",
		"bind-dynamic\n",
		"dhcp-range=192.168.99.100,192.168.99.200,12h\n",
		"no-hosts\n",
		"no-resolv\n",
		"log-facility=/dev/null\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf 缺少既有配置项 %q:\n%s", want, conf)
		}
	}
}

// TestRenderDnsmasqConfPerBridge 覆盖「两台交换机各写各的 conf」。
//
// 每个桥一个独立实例 + 独立配置：接口名与 DHCP 段都不同，不能互相复用。
func TestRenderDnsmasqConfPerBridge(t *testing.T) {
	a := renderDnsmasqConf("kbrA", "10.10.1.100", "10.10.1.200", nil)
	b := renderDnsmasqConf("kbrB", "10.20.1.100", "10.20.1.200", nil)

	if a == b {
		t.Fatal("两个交换机的配置不应相同")
	}
	for _, want := range []string{"interface=kbrA\n", "dhcp-range=10.10.1.100,10.10.1.200,12h\n"} {
		if !strings.Contains(a, want) {
			t.Errorf("kbrA 的配置缺少 %q:\n%s", want, a)
		}
	}
	for _, want := range []string{"interface=kbrB\n", "dhcp-range=10.20.1.100,10.20.1.200,12h\n"} {
		if !strings.Contains(b, want) {
			t.Errorf("kbrB 的配置缺少 %q:\n%s", want, b)
		}
	}
}

// TestStartDnsmasqRejectsImmediateExit 是本缺陷的第二处根因（假成功）。
//
// `cmd.Start()` 返回 nil 只说明 fork/exec 成功。进程随后因端口冲突退出时，
// 旧实现照样返回成功 → 任务 success、面板 active，而 DHCP 不存在。
// 这里断言：必须返回错误、错误里带桥名与线索、并且半启动的进程被收掉。
func TestStartDnsmasqRejectsImmediateExit(t *testing.T) {
	f := newFakeDnsmasq(false)
	f.exitErr = errors.New("failed to create listening socket for 127.0.0.1: Address already in use")

	err := startDnsmasqForBridgeWith("kbrSecond", f)
	if err == nil {
		t.Fatal("进程启动后立刻退出时必须返回错误（不允许假 success）")
	}
	for _, want := range []string{"kbrSecond", "立即退出", "Address already in use", "53"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q，实际: %v", want, err)
		}
	}
	if len(f.killed) != 1 {
		t.Errorf("半启动的进程必须被收掉，Kill 次数 = %d", len(f.killed))
	}
}

// TestStartDnsmasqAcceptsStableProcess 覆盖正常启动（校验窗口过后仍存活）。
func TestStartDnsmasqAcceptsStableProcess(t *testing.T) {
	f := newFakeDnsmasq(true)
	if err := startDnsmasqForBridgeWith("kbrOK", f); err != nil {
		t.Fatalf("正常启动不该失败: %v", err)
	}
	if len(f.killed) != 0 {
		t.Errorf("正常启动不该被 Kill，实际 %v", f.killed)
	}
	if len(f.started) != 1 {
		t.Fatalf("应启动 1 个实例，实际 %d", len(f.started))
	}
	for pid, conf := range f.started {
		if !strings.HasSuffix(conf, "/kbrOK.conf") {
			t.Errorf("pid %d 用的配置路径不对: %s", pid, conf)
		}
	}
}

// TestTwoBridgesBothStart 覆盖「两台交换机都能起来」——这正是真机上失败的那一步。
//
// 两台各自用自己的 conf 启动、各自通过校验；pid 不重复，conf 路径不串。
func TestTwoBridgesBothStart(t *testing.T) {
	f := newFakeDnsmasq(true)
	if err := startDnsmasqForBridgeWith("kbrOne", f); err != nil {
		t.Fatalf("第一台启动失败: %v", err)
	}
	if err := startDnsmasqForBridgeWith("kbrTwo", f); err != nil {
		t.Fatalf("第二台启动失败: %v", err)
	}
	if len(f.started) != 2 {
		t.Fatalf("应记录 2 次启动，实际 %d", len(f.started))
	}
	paths := map[string]bool{}
	for _, conf := range f.started {
		paths[conf] = true
	}
	if len(paths) != 2 {
		t.Fatalf("两台交换机必须各用各的 conf，实际 %v", paths)
	}
	for pid := range f.started {
		if pid == 0 {
			t.Error("pid 不应为 0")
		}
	}
}

// TestWaitDnsmasqAliveWindow 覆盖校验窗口本身：
// 进程一直存活 → nil；进程退出 → 立刻返回它的错误（不必等满窗口）。
func TestWaitDnsmasqAliveWindow(t *testing.T) {
	alive := newFakeDnsmasq(true)
	if err := waitDnsmasqAlive(alive, 1); err != nil {
		t.Fatalf("存活进程不该报错: %v", err)
	}

	dead := newFakeDnsmasq(false)
	dead.exitErr = errors.New("配置错误")
	if err := waitDnsmasqAlive(dead, 1); err == nil {
		t.Fatal("已退出的进程必须返回错误")
	} else if !strings.Contains(err.Error(), "配置错误") {
		t.Errorf("错误应透传进程退出原因，实际: %v", err)
	}
}
