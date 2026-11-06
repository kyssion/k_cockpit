// ex_net_write.go：网络域的写操作执行器（P3 第一批——VPC 交换机（OVS）、
// 网桥（Linux 桥）、上行口入桥（带节点侧看门狗）、网络收敛修复）。
//
// 实现口径与真机环境对齐：OVS 3.3（ovs-vsctl/ovs-ofctl）、iproute2、
// dnsmasq。安全组/防火墙/公网 IP/端口安全/镜像/抓包在 ex_net_security.go
// 与 ex_net_extra.go。
package nodeagent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// switchSpec 是 vpc.switch.change 的 spec 结构化视图。
type switchSpec struct {
	Name         string
	Mode         string
	VlanID       int
	CIDR         string
	GatewayIP    string
	DHCPStart    string
	DHCPEnd      string
	UplinkIf     string
	BandwidthIn  int
	BandwidthOut int
}

func parseSwitchSpec(params map[string]any) switchSpec {
	s := switchSpec{}
	if raw, ok := params["spec"].(map[string]any); ok {
		str := func(k string) string { v, _ := raw[k].(string); return v }
		num := func(k string) int { v, _ := raw[k].(float64); return int(v) }
		s.Name = str("name")
		s.Mode = str("mode")
		s.VlanID = num("vlan_id")
		s.CIDR = str("cidr")
		s.GatewayIP = str("gateway_ip")
		s.DHCPStart = str("dhcp_start")
		s.DHCPEnd = str("dhcp_end")
		s.UplinkIf = str("uplink_if")
		s.BandwidthIn = num("bandwidth_in_mbps")
		s.BandwidthOut = num("bandwidth_out_mbps")
	}
	return s
}

// vpcSwitchChange 实现 vpc.switch.change（create / update / delete）。
//
// OVS 桥 + internal 端口做网关 + 独立 dnsmasq（DHCP）+ NAT 出网 + 上行口入桥。
// 删除时逆序拆除（杀 dnsmasq → 摘 NAT → 摘上行口 → 删桥）。
func vpcSwitchChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	bridge := op.Target
	action := strParam(op.Params, "action")
	spec := parseSwitchSpec(op.Params)

	switch action {
	case "create", "update":
		// **上行口不能是承载控制面连接的那块网卡。**
		//
		// 把它 add-port 进 OVS 的那一刻，宿主在这个网卡上的地址就会失效——
		// 控制面与节点之间的连接会当场断掉，而这台机器上其余的虚拟机也一起
		// 失去网络。拒绝比执行后由用户去带外救援要好。
		if reason := uplinkBlockedReason(spec.UplinkIf, defaultRouteIface(ctx)); reason != "" {
			return failResult(reason), nil
		}

		report(Stage{Key: "create_bridge", Name: "创建 OVS 桥并接入上行口", Index: 1, Total: 3})
		created, err := createSwitchBridge(ctx, sys.Run, bridge, spec)
		if err != nil {
			return failResult(err.Error()), nil
		}

		report(Stage{Key: "gateway", Name: "配置网关与 DHCP", Index: 2, Total: 3})
		if spec.CIDR != "" {
			// internal 口做网关：ip addr add + link up（幂等：先删后加会闪断，
			// 用 addr replace）。
			gw := spec.GatewayIP
			if gw == "" {
				// CIDR 的第一个可用地址做网关。
				gw = firstAddrOf(spec.CIDR)
			}
			_, _ = sys.Run(ctx, "ip", "addr", "replace", gw, "dev", bridge)
			_, _ = sys.Run(ctx, "ip", "link", "set", bridge, "up")
			// DHCP：dnsmasq 绑定到该桥（listen 接口 + 动态范围）。
			if spec.DHCPStart != "" && spec.DHCPEnd != "" {
				if err := runDnsmasqForBridge(ctx, bridge, spec.DHCPStart, spec.DHCPEnd); err != nil {
					// **DHCP 起不来就等于交换机没建成**（第 3 轮 DEF-R3-15）。
					//
					// 一台拿不到地址的 NAT 交换机对用户没有任何用处，而
					// 「面板 active、实际没有 DHCP」是最坏的一种结果——比明确
					// 失败更坏，因为没人会去查。
					//
					// 只有「桥是本任务建的」才回滚：重配置（update）撞上
					// DHCP 失败时，那座桥本来就在承载业务，拆掉它等于用一次
					// 失败的改动去毁掉一条正在用的网络。那种情况直接报失败，
					// 让用户看到"配置没生效"而不是"网络没了"。
					if created {
						rollbackSwitchBridge(ctx, sys.Run, bridge, spec)
						return failResult("DHCP 启动失败: " + err.Error() +
							"（已回滚本次创建的网桥 " + bridge + "）"), nil
					}
					return failResult("DHCP 启动失败: " + err.Error() +
						"（网桥 " + bridge + " 原本已存在，未做拆除，请检查该节点上的 dnsmasq）"), nil
				}
			}
			// NAT：把该网段 MASQUERADE 到上行（幂等，已存在则跳过）。
			iptablesNatAdd(ctx, "POSTROUTING",
				"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
		}
		// 带宽总限（OVS ingress policing 简化实现：仅入向）。
		if spec.BandwidthIn > 0 {
			_, _ = sys.Run(ctx, "ovs-vsctl", "set", "interface", bridge,
				"ingress_policing_rate="+strconv.Itoa(spec.BandwidthIn*1000))
		}

		// created 告诉控制面「这座桥是不是本任务建的」：写记录失败时它据此
		// 决定能否回滚（把别人建好的桥删掉是更坏的结局）。
		return &agent.Result{
			Success: true,
			Data:    map[string]any{agent.SwitchBridgeCreatedKey: created},
		}, nil

	case "delete":
		report(Stage{Key: "destroy", Name: "拆除交换机", Index: 1, Total: 1})
		stopDnsmasqForBridge(bridge)
		_ = osRemove(dnsmasqConfPath(bridge))
		// NAT 的 MASQUERADE 随桥一起拆（真机验证抓出：此前只删桥，
		// POSTROUTING 里的规则残留——交换机没了规则还在，地址段被下一个
		// 交换机复用时会叠出双份规则）。
		if spec.CIDR != "" {
			_, _ = sys.Run(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING",
				"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
		}
		// 上行口要**显式摘掉**：桥被删时 OVS 会把端口一并释放，但
		// `--if-exists del-port` 让"桥还在、口还挂着"的中间态也能被收敛，
		// 也让重复执行删除保持幂等。
		if err := detachSwitchBridge(ctx, sys.Run, bridge, spec.UplinkIf); err != nil {
			return failResult("删除 OVS 桥失败: " + err.Error()), nil
		}

	default:
		return failResult("未知交换机动作: " + action), nil
	}
	return &agent.Result{Success: true}, nil
}

// cmdRunner 是一条系统命令的执行入口；生产实现固定是 sys.Run。
//
// 把它做成参数而不是直接调 sys.Run，是为了让「发出哪些命令、按什么顺序、
// 失败后回滚了没有」这些**可以用单测钉住**——它们正是这一批缺陷的全部内容
// （命令写错、字段没被使用、失败没回滚），而真机回归只能事后发现。
type cmdRunner func(ctx context.Context, name string, args ...string) (string, error)

// uplinkBlockedReason 判断上行口是否就是承载默认路由的网卡；是则返回拒绝文案。
//
// 抽成纯函数（只比较两个字符串）是为了可测：判断逻辑在这里，取默认路由网卡
// 的命令调用留在调用方。
func uplinkBlockedReason(uplinkIf, defaultIface string) string {
	if uplinkIf == "" || defaultIface == "" || uplinkIf != defaultIface {
		return ""
	}
	return "上行口 " + uplinkIf + " 是该节点承载默认路由的网卡（控制面连接走它）。" +
		"把它接入 OVS 桥会立刻切断本节点的管理连接与既有虚拟机的网络。" +
		"请改用专用的上行网卡；确实要走 OVS 上联，请在带外控制台上完成并自行承担切换风险。"
}

// createSwitchBridge 建桥 → 设 VLAN 模式 → 接入上行口。
//
// 返回值 created 表示这座桥在本次调用**之前并不存在**（由本任务创建）。
// 它决定控制面在写记录失败时能否删除该桥。
//
// 上行口接入失败时**回滚刚建的桥**：否则一次失败的创建会在宿主机上留下一个
// 空桥，而它没有任何记录可以追踪（真机上就是 kbr056539e3 那一类残留）。
// 回滚只删桥，**不碰上行口设备本身**——那块网卡不属于我们建的东西。
func createSwitchBridge(
	ctx context.Context, run cmdRunner, bridge string, spec switchSpec,
) (created bool, err error) {
	// br-exists 在桥不存在时返回非零，因此「报错」正是"需要新建"。
	_, brErr := run(ctx, "ovs-vsctl", "br-exists", bridge)
	created = brErr != nil

	if _, err := run(ctx, "ovs-vsctl", "--may-exist", "add-br", bridge); err != nil {
		return created, fmt.Errorf("创建 OVS 桥失败: %w", err)
	}
	if spec.VlanID > 0 {
		_, _ = run(ctx, "ovs-vsctl", "set", "bridge", bridge, "other_config:vlan-mode=access")
	}

	if spec.UplinkIf == "" {
		// 隔离/仅 NAT 的交换机没有上行口：到此为止，不算错误。
		return created, nil
	}
	if err := attachSwitchUplink(ctx, run, bridge, spec.UplinkIf); err != nil {
		// 同样只在「桥是本任务建的」时回滚：重配置一座在用的桥时接上行口
		// 失败，拆掉整座桥会把上面所有虚拟机的网络一起断掉——那是用一次
		// 失败的改动换来一次断网。
		if created {
			rollbackSwitchBridge(ctx, run, bridge, spec)
			return created, fmt.Errorf("上行口接入失败: %w（已回滚网桥 %s）", err, bridge)
		}
		return created, fmt.Errorf("上行口接入失败: %w（网桥 %s 原本已存在，未做拆除）", err, bridge)
	}
	return created, nil
}

// rollbackSwitchBridge 拆掉一次**未完成的创建**留下的东西。
//
// 顺序与 delete 分支一致：停 dnsmasq → 删 conf → 摘 NAT → 摘上行口 → 删桥。
// 抽成函数是因为它现在有两个调用点（上行口接入失败、DHCP 启动失败），
// 两处各写一遍的话，迟早有一处漏掉某一步——而漏掉的那一步就是下一次
// 「悬空桥/悬空 NAT 规则」的来源。
//
// 只拆本次创建的东西：不碰任何设备（上行口只从桥上摘下来）、不碰别的桥。
func rollbackSwitchBridge(ctx context.Context, run cmdRunner, bridge string, spec switchSpec) {
	stopDnsmasqForBridge(bridge)
	_ = osRemove(dnsmasqConfPath(bridge))
	if spec.CIDR != "" {
		_, _ = run(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING",
			"-s", spec.CIDR, "!", "-o", bridge, "-j", "MASQUERADE")
	}
	if err := detachSwitchBridge(ctx, run, bridge, spec.UplinkIf); err != nil {
		log.Printf("[nodeagent] 回滚网桥 %s 时删除失败: %v", bridge, err)
	}
}

// attachSwitchUplink 把物理上行口接入 OVS 桥。
//
// **必须用 ovs-vsctl add-port**：这是 OVS 桥，`ip link set <if> master <br>`
// 是 Linux 网桥的做法，对 OVS 桥不生效（命令甚至可能成功返回，而端口根本
// 没被 OVS 管起来——这正是本缺陷"看起来做了、其实没做"的形状）。
//
// `--may-exist` 让重复 create（例如重配置）成为幂等操作：端口已存在时
// 不报错、也不重建，避免把正在跑的流量抖断。
func attachSwitchUplink(ctx context.Context, run cmdRunner, bridge, uplinkIf string) error {
	if _, err := run(ctx, "ovs-vsctl", "--may-exist", "add-port", bridge, uplinkIf); err != nil {
		return err
	}
	// 物理口被 OVS 接管后是 down 的，必须显式拉起，否则桥内也不通。
	if _, err := run(ctx, "ip", "link", "set", uplinkIf, "up"); err != nil {
		return err
	}
	return nil
}

// detachSwitchBridge 摘上行口 → 删 internal 口 → 删桥。
func detachSwitchBridge(ctx context.Context, run cmdRunner, bridge, uplinkIf string) error {
	if uplinkIf != "" {
		_, _ = run(ctx, "ovs-vsctl", "--if-exists", "del-port", bridge, uplinkIf)
	}
	_, _ = run(ctx, "ip", "link", "del", bridge) // OVS 桥删除后 internal 口随之消失
	if _, err := run(ctx, "ovs-vsctl", "--if-exists", "del-br", bridge); err != nil {
		return err
	}
	return nil
}

// runDnsmasqForBridge 在桥上启动独立 dnsmasq（pid 文件幂等：已跑则先停）。
func runDnsmasqForBridge(_ context.Context, bridge, start, end string) error {
	// 保留已有静态租约：重配置交换机不能把用户绑定的静态地址清掉。
	hosts := readDnsmasqHosts(bridge)
	if err := writeDnsmasqConf(bridge, start, end, hosts); err != nil {
		return err
	}
	return startDnsmasqForBridge(bridge)
}

func dnsmasqConfPath(bridge string) string {
	return "/var/lib/k_cockpit/dnsmasq/" + bridge + ".conf"
}

// writeDnsmasqConf 写基础配置与静态租约。
func writeDnsmasqConf(bridge, start, end string, hosts []string) error {
	_ = osMkdirAllErr("/var/lib/k_cockpit/dnsmasq")
	return osWriteFile(dnsmasqConfPath(bridge),
		[]byte(renderDnsmasqConf(bridge, start, end, hosts)), 0o600)
}

// renderDnsmasqConf 渲染一个交换机专属的 dnsmasq 配置。
//
// **`port=0` 是必须的**（第 3 轮 DEF-R3-15）：dnsmasq 默认会监听
// `127.0.0.1:53` 做 DNS。本项目每个交换机起一个独立实例、只做 DHCP，
// 第二台实例必然在 53 端口上撞车——`failed to create listening socket for
// 127.0.0.1: Address already in use`，然后当场退出。关闭 DNS 之后每个实例
// 只占自己的桥，互不干扰，也不再占用宿主机的 53 端口（那是 systemd-resolved
// 或用户自建 DNS 的地方，本来就不该被我们抢）。
//
// 抽成纯函数是为了单测能直接断言配置内容（含 port=0），不必真的去写
// /var/lib 下的文件。
func renderDnsmasqConf(bridge, start, end string, hosts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "interface=%s\n", bridge)
	b.WriteString("bind-dynamic\n")
	// 只做 DHCP，不做 DNS。
	b.WriteString("port=0\n")
	fmt.Fprintf(&b, "dhcp-range=%s,%s,12h\n", start, end)
	fmt.Fprintf(&b, "dhcp-option=option:router,%s\n", gatewayGuess(start))
	b.WriteString("no-hosts\nno-resolv\nlog-facility=/dev/null\n")
	for _, h := range hosts {
		if strings.TrimSpace(h) != "" {
			b.WriteString(strings.TrimSpace(h) + "\n")
		}
	}
	return b.String()
}

// readDnsmasqHosts 取配置里已有的 dhcp-host 行。
func readDnsmasqHosts(bridge string) []string {
	b, err := osReadFile(dnsmasqConfPath(bridge))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(b, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "dhcp-host=") {
			out = append(out, line)
		}
	}
	return out
}

// dnsmasqPidPath 是该桥 dnsmasq 实例的 pid 文件路径。
//
// 不给 dnsmasq 传 --pid-file：前台模式（--no-daemon）实测**不写 pid 文件**
// （真机验证），靠它写等于没有——pid 由 agent 自己落盘。
func dnsmasqPidPath(bridge string) string { return "/run/kc-dnsmasq-" + bridge + ".pid" }

// dnsmasqProbe* 是"启动后校验"的观察窗口：3 次 × 100ms。
//
// 依据：dnsmasq 的失败都在**事件循环开始之前**发生（绑定端口、读配置、
// 解析 interface=），实测是毫秒级；100ms 足够让它跑到那一步。取 3 次是为了
// 容忍调度抖动与慢磁盘上的 fork/exec，同时把创建交换机的额外耗时压在
// 300ms 这个量级（用户点一次"创建"感知不到）。
//
// 这是「早期失败探测」，不是长期健康检查：进程若在窗口之后才死（例如被
// OOM），本次仍会返回成功——那种情况由下一次重配置/删除暴露，见
// 03-consistency-review.md 的残留清单。
const (
	dnsmasqProbeAttempts = 3
	dnsmasqProbeInterval = 100 * time.Millisecond
)

// dnsmasqRunner 是「起进程 / 探活 / 收尸」的抽象。
//
// 做成接口的唯一目的是让"起得来但立刻退出"这种形状可以被单测覆盖——
// 它正是 DEF-R3-15 的假成功来源，而真机复现要靠手工碰端口冲突。
type dnsmasqRunner interface {
	// Start 启动一个实例，返回 pid。
	Start(confPath string) (int, error)
	// Alive 报告进程是否仍在运行。
	Alive(pid int) bool
	// ExitErr 返回已退出进程的错误；仍在运行时返回 nil。
	ExitErr(pid int) error
	// Kill 收掉进程（清理半启动的实例）。
	Kill(pid int)
}

// startDnsmasqForBridge 用配置文件启动 dnsmasq，并**确认它真的活着**。
func startDnsmasqForBridge(bridge string) error {
	return startDnsmasqForBridgeWith(bridge, realDnsmasq)
}

// startDnsmasqForBridgeWith 是可注入 runner 的实现（见 dnsmasqRunner）。
func startDnsmasqForBridgeWith(bridge string, run dnsmasqRunner) error {
	confPath := dnsmasqConfPath(bridge)
	pidFile := dnsmasqPidPath(bridge)

	// 重启前先停旧实例（幂等：没有 pid 文件就什么都不做）。
	//
	// **刻意不用请求 ctx**：此前绑定在 gRPC 会话 ctx 上——agent 一断流重连，
	// 全部交换机的 DHCP 同时被杀且无人拉起（review 抓出）。DHCP 的生命周期
	// 属于桥，不属于某次下发；停机由 stopDnsmasqForBridge（删除/重配置
	// 交换机的路径）负责。
	stopDnsmasqForBridge(bridge)

	pid, err := run.Start(confPath)
	if err != nil {
		return fmt.Errorf("启动 dnsmasq 失败（网桥 %s）: %w", bridge, err)
	}
	if err := osWriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		// pid 落不了盘不阻断：pidfile 只是停机时找进程的索引，而这一次的
		// 结论取决于"实例是否真的活着"。
		log.Printf("[nodeagent] 写 dnsmasq pidfile 失败 bridge=%s pid=%d: %v", bridge, pid, err)
	}

	// **启动后校验**（第 3 轮 DEF-R3-15 的假成功）。
	//
	// `cmd.Start()` 返回 nil 只说明 fork/exec 成功。进程随后因端口冲突、
	// 配置错误退出时，Start 依然是 nil——旧实现据此返回成功，于是任务
	// success、面板 active，而 DHCP 根本不存在。用户拿到的是一台"配好了
	// 但拿不到地址"的交换机，比一次明确的失败难查得多。
	if err := waitDnsmasqAlive(run, pid); err != nil {
		run.Kill(pid)
		_ = osRemove(pidFile)
		return fmt.Errorf(
			"dnsmasq 在网桥 %s 上启动后立即退出: %v。"+
				"常见原因：53 端口被别的 dnsmasq 占用（本项目已用 port=0 关闭 DNS，"+
				"若仍出现请检查宿主机是否有外部 dnsmasq / systemd-resolved 抢占）、"+
				"或网桥尚不存在、配置行非法", bridge, err)
	}
	return nil
}

// waitDnsmasqAlive 用固定观察窗口确认进程没在启动后立刻死掉。
//
// 窗口内每 100ms 复查一次；一旦发现已退出就立即返回它的错误（不必等满
// 窗口）。窗口用完仍存活才返回 nil——因此"活过 300ms"是这一层的判定标准。
func waitDnsmasqAlive(run dnsmasqRunner, pid int) error {
	for i := 0; i < dnsmasqProbeAttempts; i++ {
		time.Sleep(dnsmasqProbeInterval)
		if run.Alive(pid) {
			continue
		}
		if err := run.ExitErr(pid); err != nil {
			return err
		}
		return errors.New("进程已退出")
	}
	return nil
}

// execDnsmasq 是 dnsmasqRunner 的生产实现。
type execDnsmasq struct {
	mu    sync.Mutex
	procs map[int]*dnsmasqProc
}

type dnsmasqProc struct {
	cmd    *exec.Cmd
	exited bool
	err    error
}

// realDnsmasq 是生产用的单例：进程表要跨调用保留，否则 Wait 的结果无处存放。
var realDnsmasq = &execDnsmasq{procs: map[int]*dnsmasqProc{}}

func (e *execDnsmasq) Start(confPath string) (int, error) {
	cmd := exec.Command("dnsmasq", "--no-daemon", "--conf-file="+confPath)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	e.mu.Lock()
	if e.procs == nil {
		e.procs = map[int]*dnsmasqProc{}
	}
	rec := &dnsmasqProc{cmd: cmd}
	e.procs[pid] = rec
	e.mu.Unlock()

	// 必须有人 Wait，否则子进程变成僵尸、状态永远读不到。
	go func() {
		err := cmd.Wait()
		e.mu.Lock()
		rec.exited, rec.err = true, err
		e.mu.Unlock()
	}()
	return pid, nil
}

func (e *execDnsmasq) Alive(pid int) bool {
	e.mu.Lock()
	rec, ok := e.procs[pid]
	exited := ok && rec.exited
	e.mu.Unlock()
	if exited {
		return false
	}
	// kill -0：进程存在即成功（不发信号）。已 Wait 的进程在此前已被判掉。
	return syscall.Kill(pid, 0) == nil
}

func (e *execDnsmasq) ExitErr(pid int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rec, ok := e.procs[pid]; ok && rec.exited {
		return rec.err
	}
	return nil
}

func (e *execDnsmasq) Kill(pid int) {
	e.mu.Lock()
	rec, ok := e.procs[pid]
	e.mu.Unlock()
	if !ok {
		return
	}
	if rec.cmd.Process != nil {
		_ = rec.cmd.Process.Kill()
	}
	e.mu.Lock()
	delete(e.procs, pid)
	e.mu.Unlock()
}

// upsertDHCPHost 增加/替换一条 DHCP 静态租约并重启该桥的 dnsmasq。
//
// 配置文件不存在时用控制面下发的范围补建：交换机可能是旧版本 agent
// 创建的，当时还没有 conf 文件。
func upsertDHCPHost(ctx context.Context, bridge, mac, ip, start, end string) error {
	path := dnsmasqConfPath(bridge)
	b, err := osReadFile(path)
	if err != nil {
		if start == "" || end == "" {
			return fmt.Errorf("网桥 %s 没有由平台管理的 DHCP 配置: %w", bridge, err)
		}
		if err := writeDnsmasqConf(bridge, start, end, nil); err != nil {
			return err
		}
		b, err = osReadFile(path)
		if err != nil {
			return err
		}
	}
	want := "dhcp-host=" + strings.ToLower(mac) + "," + ip
	var lines []string
	for _, line := range strings.Split(b, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "dhcp-host=") &&
			(strings.Contains(strings.ToLower(trim), strings.ToLower(mac)+",") ||
				strings.HasSuffix(trim, ","+ip)) {
			continue
		}
		lines = append(lines, trim)
	}
	lines = append(lines, want)
	// 用原有的 dhcp-range 重写：从配置文本里取，避免再问控制面。
	rangeStart, rangeEnd := dhcpRangeOf(lines)
	if rangeStart == "" || rangeEnd == "" {
		return fmt.Errorf("网桥 %s 的 DHCP 配置缺少 dhcp-range", bridge)
	}
	if err := writeDnsmasqConf(bridge, rangeStart, rangeEnd, filterHostLines(lines)); err != nil {
		return err
	}
	return startDnsmasqForBridge(bridge)
}

// removeDHCPHost 删除一条静态租约；配置不存在时按幂等成功处理。
func removeDHCPHost(_ context.Context, bridge, mac string) error {
	path := dnsmasqConfPath(bridge)
	b, err := osReadFile(path)
	if err != nil {
		return nil
	}
	var kept []string
	start, end := "", ""
	for _, line := range strings.Split(b, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "dhcp-range=") {
			start, end = parseDHCPRange(trim)
			kept = append(kept, trim)
			continue
		}
		if strings.HasPrefix(trim, "dhcp-host=") &&
			strings.Contains(strings.ToLower(trim), strings.ToLower(mac)+",") {
			continue
		}
		kept = append(kept, trim)
	}
	if start == "" || end == "" {
		return nil
	}
	if err := writeDnsmasqConf(bridge, start, end, filterHostLines(kept)); err != nil {
		return err
	}
	return startDnsmasqForBridge(bridge)
}

func filterHostLines(lines []string) []string {
	var out []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "dhcp-host=") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func parseDHCPRange(line string) (string, string) {
	v := strings.TrimPrefix(line, "dhcp-range=")
	parts := strings.Split(v, ",")
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func dhcpRangeOf(lines []string) (string, string) {
	for _, line := range lines {
		if strings.HasPrefix(line, "dhcp-range=") {
			return parseDHCPRange(line)
		}
	}
	return "", ""
}

// stopDnsmasqForBridge 停掉桥对应的 dnsmasq。
//
// 杀前必须核验进程身份（AGENTS.md 杀进程纪律）：pid 文件在 /run，
// dnsmasq 崩溃后 PID 被系统复用时，裸 kill 会杀掉一个毫不相干的进程。
// /proc/<pid>/comm 不是 dnsmasq 一律不动（残留 pid 文件顺带清掉）。
func stopDnsmasqForBridge(bridge string) {
	pidFile := dnsmasqPidPath(bridge)
	b, err := osReadFile(pidFile)
	if err != nil {
		return
	}
	pid := strings.TrimSpace(b)
	if pid == "" {
		return
	}
	comm, cerr := osReadFile("/proc/" + pid + "/comm")
	if cerr != nil || strings.TrimSpace(comm) != "dnsmasq" {
		_ = osRemove(pidFile) // 陈旧 pid 文件：进程已不在或被复用，只清文件不杀
		return
	}
	_ = exec.Command("kill", pid).Run()
	_ = osRemove(pidFile)
}

// gatewayGuess 从 DHCP 起址猜网关（.1 惯例仅展示用；真实网关取自网关 IP 配置）。
func gatewayGuess(start string) string {
	parts := strings.Split(start, ".")
	if len(parts) != 4 {
		return start
	}
	return parts[0] + "." + parts[1] + "." + parts[2] + ".1"
}

// firstAddrOf 取 CIDR 的第一个可用主机地址做网关。
func firstAddrOf(cidr string) string {
	// 简化实现：10.0.0.0/24 → 10.0.0.1。
	base := strings.SplitN(cidr, "/", 2)[0]
	parts := strings.Split(base, ".")
	if len(parts) != 4 {
		return base
	}
	last, err := strconv.Atoi(parts[3])
	if err != nil {
		return base
	}
	parts[3] = strconv.Itoa(last + 1)
	return strings.Join(parts, ".")
}

// networkBridgeApply 实现 network.bridge.apply（Linux 基础桥）。
func networkBridgeApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "create", Name: "创建基础桥", Index: 1, Total: 1})
	name := op.Target
	if _, err := sys.Run(ctx, "ip", "link", "add", "name", name, "type", "bridge"); err != nil {
		// 已存在按成功（幂等收敛）。
		if !strings.Contains(err.Error(), "exist") {
			return failResult(err.Error()), nil
		}
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", name, "up")
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Fixed: []string{name}, Message: "基础桥已就绪"},
	}}, nil
}

// networkBridgeDelete 实现 network.bridge.delete。
func networkBridgeDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "delete", Name: "删除基础桥", Index: 1, Total: 1})
	if _, err := sys.Run(ctx, "ip", "link", "del", op.Target); err != nil {
		if !strings.Contains(err.Error(), "Cannot find") && !strings.Contains(err.Error(), "not found") {
			return failResult(err.Error()), nil
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Message: "基础桥已删除"},
	}}, nil
}

// networkUplinkAttach 实现 network.uplink.attach——**最危险操作**：
// 物理口入桥 + 节点侧看门狗（到点未确认自动摘出）。
func networkUplinkAttach(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	bridge := strParam(op.Params, "bridge")
	iface := strParam(op.Params, "uplink_if")
	seconds := 0
	if v, ok := op.Params["watchdog_seconds"].(float64); ok {
		seconds = int(v)
	}
	report(Stage{Key: "watch_arm", Name: "布防自动回滚看门狗", Index: 1, Total: 2})
	// **看门狗先布防再操作**（阶段顺序契约）：把摘口命令交给延时执行器，
	// 操作失败时主动解除。
	cancel := armUplinkWatchdog(bridge, iface, time.Duration(seconds)*time.Second)

	report(Stage{Key: "apply", Name: "物理口入桥", Index: 2, Total: 2})
	if _, err := sys.Run(ctx, "ip", "link", "set", iface, "nomaster"); err != nil {
		cancel()
		return failResult("物理口脱离原 master 失败: " + err.Error()), nil
	}
	if _, err := sys.Run(ctx, "ip", "link", "set", iface, "master", bridge); err != nil {
		cancel()
		return failResult("物理口入桥失败: " + err.Error()), nil
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", bridge, "up")
	// 看门狗保持布防：控制面在窗口内调 confirm 才解除。
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{
			Message: fmt.Sprintf("物理口 %s 已入桥 %s，看门狗 %d 秒", iface, bridge, seconds),
		},
	}}, nil
}

// armUplinkWatchdog 布防节点侧看门狗：到点未确认（confirm 文件未写）就
// 自动摘口。返回解除函数。
func armUplinkWatchdog(bridge, iface string, d time.Duration) (cancel func()) {
	confirmFile := "/run/kc-uplink-confirm-" + bridge
	_ = osRemove(confirmFile)
	t := time.AfterFunc(d, func() {
		if _, err := osStat(confirmFile); err == nil {
			return // 已确认
		}
		// 自动回滚：摘口。
		ctx, cancel2 := contextWithTimeout(5 * timeSecond)
		defer cancel2()
		_, _ = sys.Run(ctx, "ip", "link", "set", iface, "nomaster")
	})
	return func() {
		t.Stop()
		_ = osWriteFile(confirmFile, []byte("ok"), 0o644)
	}
}

// networkUplinkConfirm 实现 network.uplink.confirm（取消自动回滚）。
func networkUplinkConfirm(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "confirm", Name: "确认保持", Index: 1, Total: 1})
	_ = osWriteFile("/run/kc-uplink-confirm-"+op.Target, []byte("ok"), 0o644)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Message: "自动回滚已取消，物理口保持入桥"},
	}}, nil
}

// networkUplinkDetach 实现 network.uplink.detach（摘口并恢复）。
func networkUplinkDetach(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	iface := strParam(op.Params, "uplink_if")
	report(Stage{Key: "detach", Name: "摘出物理口", Index: 1, Total: 1})
	if _, err := sys.Run(ctx, "ip", "link", "set", iface, "nomaster"); err != nil {
		return failResult(err.Error()), nil
	}
	_, _ = sys.Run(ctx, "ip", "link", "set", iface, "up")
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Message: "物理口已摘出并恢复 UP"},
	}}, nil
}

// networkRepair 实现 network.repair（幂等收敛：基础桥全部 up、OVS 服务在跑）。
func networkRepair(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "converge", Name: "核对网络现状", Index: 1, Total: 1})
	var fixed, remaining []string
	if !sys.OVSRunning(ctx) {
		if sys.RunOK(ctx, "systemctl", "start", "ovs-vswitchd") {
			fixed = append(fixed, "ovs-vswitchd 已拉起")
		} else {
			remaining = append(remaining, "ovs-vswitchd 无法启动（检查 OVS 安装）")
		}
	}
	links, _ := sys.ListLinks(ctx)
	for _, l := range links {
		if strings.HasPrefix(l.IfName, "br-") && l.OperState != "UP" {
			_, _ = sys.Run(ctx, "ip", "link", "set", l.IfName, "up")
			fixed = append(fixed, l.IfName+" 已置 UP")
		}
	}
	msg := "网络收敛完成"
	if len(remaining) > 0 {
		msg = "仍有需要人工处理的项目"
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NetworkRepairKey: agent.NetworkRepairInfo{Fixed: fixed, Remaining: remaining, Message: msg},
	}}, nil
}
