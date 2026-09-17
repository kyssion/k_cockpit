package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math"
	"time"
)

// MockClient 是 Client 的假实现。
//
// 它**只保证接口有返回值**，不模拟节点行为：不模拟耗时、进度推进、失败注入
// 与离线（见 docs/06-decisions/0007-mock-agent-first.md）。接入真实 agent 前，
// 执行失败、超时、节点离线等分支不会被触发，需在联调时集中验证。
//
// 有一个例外：**阶段会上报**。阶段不是「节点行为的模拟」，而是节点向控制面
// 传递信息的一种方式——不上报的话，本项目的任务时间线在接入真实 agent 之前
// 根本没有任何数据流过，那条链路（模型、写入、查询、渲染）就始终是未验证的。
// 因此这里按每类操作的真实步骤上报，让整条链路在 mock 下也能被走通和验收。
type MockClient struct {
	// StageDelay 是相邻两个阶段之间的等待。
	//
	// 零值表示不等待——**测试需要的是确定性，不是真实感**，让每个用例多等
	// 一秒只会让人不愿跑测试。服务端启动时会设一个非零值，好让演示时能看见
	// 时间线逐步推进，而不是所有阶段在同一毫秒里一起出现。
	StageDelay time.Duration
}

// NewMockClient 构造假实现（阶段之间不等待）。
func NewMockClient() *MockClient { return &MockClient{} }

// WithStageDelay 返回一份带阶段等待的副本。
func (m *MockClient) WithStageDelay(d time.Duration) *MockClient {
	return &MockClient{StageDelay: d}
}

// stagePlan 返回某类操作在节点上实际经历的步骤。
//
// 步骤写得贴近真实流程而不是「步骤一/步骤二」这类占位名：时间线的全部价值
// 在于让人看出**卡在哪一步**，而占位名会让这一栏永远没有信息量。
//
// 未登记的操作返回 nil，调用方照常执行、只是没有阶段——这比编几个通用步骤
// 要好：一个显示「执行中 → 完成」的时间线不提供任何定位能力，却会让人以为
// 阶段是齐全的。
func stagePlan(kind OpKind) [][2]string {
	switch kind {
	case OpVMCreate:
		return [][2]string{
			{"resource_check", "校验宿主机资源"},
			{"disk_allocate", "分配磁盘"},
			{"domain_define", "生成域配置"},
			{"domain_start", "定义并启动"},
		}
	case OpVMDelete:
		return [][2]string{
			{"power_off", "停止虚拟机"},
			{"disk_detach", "断开磁盘"},
			{"disk_destroy", "删除磁盘"},
			{"domain_undefine", "清理域配置"},
		}
	case OpVMSnapshotCreate:
		return [][2]string{
			{"guest_freeze", "冻结文件系统"},
			{"disk_snapshot", "创建磁盘快照"},
			{"guest_thaw", "解冻文件系统"},
			{"metadata_write", "记录快照元数据"},
		}
	case OpVMSnapshotRestore:
		return [][2]string{
			{"power_off", "停止虚拟机"},
			{"disk_rollback", "回滚磁盘"},
			{"config_restore", "恢复配置"},
			{"domain_start", "启动虚拟机"},
		}
	case OpVMSnapshotDelete:
		return [][2]string{
			{"snapshot_remove", "删除快照文件"},
			{"metadata_remove", "清理元数据"},
		}
	case OpStoragePoolCreate:
		return [][2]string{
			{"device_format", "格式化设备"},
			{"pool_mount", "挂载存储池"},
			{"pool_register", "登记到节点"},
		}
	case OpStoragePoolDelete:
		return [][2]string{
			{"pool_unmount", "卸载存储池"},
			{"pool_data_remove", "删除池数据"},
			{"dir_cleanup", "清理目录"},
		}
	case OpVMConfigUpdate:
		return [][2]string{
			{"config_write", "写入域配置"},
			{"config_apply", "应用变更"},
		}
	case OpVMInterfaceChange:
		return [][2]string{
			{"config_write", "更新域配置"},
			{"nic_hotplug", "热插拔网卡"},
		}
	case OpVMStaticIPChange:
		return [][2]string{
			{"lease_write", "写入 DHCP 租约"},
			{"lease_reload", "重载生效"},
		}
	case OpVMPortForwardChange:
		return [][2]string{
			{"rule_write", "写入转发规则"},
			{"firewall_apply", "应用防火墙规则"},
		}
	case OpVMRescueEnter:
		// 救援进入：先改配置再启动。顺序反了会让虚拟机先按原配置起来，
		// 而那时已经是「救援中」的状态——用户连上去看到的还是自己的系统。
		return [][2]string{
			{"boot_config", "调整为救援引导"},
			{"device_swap", "切换盘型与网卡"},
			{"domain_define", "重写域配置"},
			{"domain_start", "从救援镜像启动"},
		}
	case OpVMRescueExit:
		return [][2]string{
			{"device_restore", "还原盘型与网卡"},
			{"boot_config", "还原引导顺序"},
			{"domain_define", "重写域配置"},
		}
	case OpVPCSwitchChange:
		return [][2]string{
			{"bridge_create", "创建网桥"},
			{"vlan_config", "配置 VLAN 与地址"},
			{"dhcp_config", "配置 DHCP 服务"},
			{"nat_config", "配置出网转发"},
		}
	case OpTemplatePrepare:
		return [][2]string{
			{"disk_clone", "复制系统盘"},
			{"disk_convert", "转换为模板格式"},
			{"template_register", "登记模板"},
		}
	case OpTemplateDelete:
		return [][2]string{
			{"disk_remove", "删除模板盘"},
		}
	case OpVMClone:
		return [][2]string{
			{"disk_clone", "克隆系统盘"},
			{"domain_define", "生成域配置"},
			{"domain_start", "定义并启动"},
		}
	}
	// 电源类操作是单步的，但仍然会上报一项：否则界面上这类任务的时间线是
	// 空的，而「空空如也」与「不支持展示」看起来是同一件事。
	switch kind {
	case OpVMStart, OpVMShutdown, OpVMPoweroff, OpVMReboot, OpVMReset:
		return [][2]string{{"node_exec", "节点执行"}}
	}
	return nil
}

// Execute 直接返回成功，不产生任何副作用。
//
// 对需要返回值的操作给出形状合理的假数据（如创建虚拟机返回 UUID），
// 否则上层拿不到它需要的东西，会在业务代码里被迫写 mock 专用的兜底分支
// ——那正是 ADR-0007 要避免的「业务代码感知 mock」。
func (m *MockClient) Execute(ctx context.Context, op Operation) (*Result, error) {
	m.reportStages(ctx, op)

	data := map[string]any{}

	switch op.Kind {
	case OpVMCreate:
		data["uuid"] = mockUUID(op.NodeID, op.Target)

	case OpVMClone:
		data["uuid"] = mockUUID(op.NodeID, op.Target)
		// 只有链式克隆才有 backing_path。完整克隆**刻意不给**：给了会让
		// 控制面记下一条不存在的依赖，界面上就会显示出一个假的依赖链——
		// 而依赖链的存在与否，决定了删除模板时该不该拒绝。
		info := CloneInfo{DiskPath: "/var/lib/k_cockpit/images/" + op.Target + ".qcow2"}
		if mode, ok := op.Params["clone_mode"].(string); ok && mode == "linked" {
			info.BackingPath, _ = op.Params["template_disk_path"].(string)
		}
		data[CloneDataKey] = info

	case OpTemplatePrepare:
		data[TemplateDataKey] = TemplateInfo{
			DiskPath: "/var/lib/k_cockpit/templates/" + op.Target + ".qcow2",
			SizeGB:   20,
			Format:   "qcow2",
		}

	case OpTemplateDelete:
		// 没有返回值：删除的结果只由 Success 表达。
	case OpNodeDisks:
		// 返回三块有代表性的盘，覆盖界面需要处理的三种状态：系统盘
		// （不可选）、空闲盘（可直接用）、已含数据的盘（需显式确认）。
		//
		// 只给"全都是空闲盘"的假数据会让「存在数据」这条分支永远不被
		// 前端渲染到，而那恰恰是最需要用户看清的一条。
		data[DiskListKey] = []Disk{
			{
				DeviceID:   "ata-mock-system",
				Path:       "/dev/sda",
				SizeBytes:  64 << 30,
				IsSystem:   true,
				Mounted:    true,
				Filesystem: "ext4",
				MountPoint: "/",
			},
			{
				DeviceID:  "ata-mock-data1",
				Path:      "/dev/sdb",
				SizeBytes: 512 << 30,
			},
			{
				DeviceID:   "ata-mock-data2",
				Path:       "/dev/sdc",
				SizeBytes:  1024 << 30,
				HasData:    true,
				Filesystem: "ext4",
			},
		}

	case OpNodeNetwork:
		// 上报「基础能力齐全、OVS 缺失」：这恰好是 M2 的典型形态，也让
		// 界面必须处理「非必需能力缺失」与「降级」两种不同的呈现——
		// 只返回「全都可用」会让这条分支永远不被渲染到。
		data[NetworkKey] = NetworkBackend{
			Mode: ModeBasic,
			Capabilities: []string{
				CapabilityBridgeBasic,
				CapabilityDHCP,
				CapabilityNAT,
			},
			Missing: map[string]string{
				CapabilityOVS: "未检测到 Open vSwitch",
			},
		}

	case OpStoragePoolCreate:
		data["mount_path"] = "/var/lib/k_cockpit/pools/" + op.Target
		data[StatusDataKey] = "ready"

	case OpVMStatus:
		// 固定返回 running（取值与 model.VMStatusRunning 一致；本包不引用
		// model —— 协议层与存储层保持解耦，状态的解释由调用方负责）。
		//
		// 由此产生的局限需要明确：mock 不维护状态，探测结果不随操作变化，
		// 因此**只能验证状态机与拒绝路径**（对 running 的虚拟机执行 start
		// 会被拒绝），**无法验证「先关机再开机」的完整流转**。要打通完整
		// 流转需要 mock 维护一份状态，那正是 ADR-0007 明确不做的事。
		data[StatusDataKey] = "running"

	case OpVMStats:
		// 指标随目标名变化，但**同一台机器每次拿到同一组值**：mock 不维护
		// 状态，若每帧都随机，界面上的 CPU 曲线会自己抖动起来——那看起来
		// 像真实负载，而实际上什么都没发生，演示时会误导人。
		data[StatsDataKey] = mockStats(op.Target)

	case OpVMConsoleFrame:
		data[FrameDataKey] = mockFrame(op.Target, 320, 180)

	case OpVMSnapshotCreate:
		// 回显控制面给的标识，让调用方走完整的「写入 domain_name」路径。
		//
		// 不在这里自己生成：真实 agent 会把它实际使用的名字返回，
		// 而控制面必须能处理「返回的名字与请求的不同」这一情况。
		if name, ok := op.Params["domain_name"].(string); ok {
			data["domain_name"] = name
		}
		// 给一个非零体积：默认 0 会让界面上的「0 B」看起来像没创建成功，
		// 而这个模拟值正好用来验证体积的展示与格式化。
		data["size_bytes"] = float64(256 * 1024 * 1024)
	}

	return &Result{
		Success: true,
		Message: "mock: " + string(op.Kind) + " 已执行",
		Data:    data,
	}, nil
}

// reportStages 按操作类型上报阶段。
//
// 在真正返回之前一次性报完，而不是边做边报：mock 本身不做任何事，把阶段
// 分散到「执行过程中」需要伪造一套并发时序，而那是 ADR-0007 明确不做的。
// 调用方按「开始即记录、下一个开始时收尾上一个」处理，因此顺序上报得到的
// 时间线形状与真实节点一致。
//
// ctx 取消时立即停止：任务被取消后还继续报阶段，会让界面在「已取消」之后
// 又冒出新的步骤。
func (m *MockClient) reportStages(ctx context.Context, op Operation) {
	if op.OnStage == nil {
		return
	}
	plan := stagePlan(op.Kind)
	for i, s := range plan {
		select {
		case <-ctx.Done():
			return
		default:
		}
		op.OnStage(Stage{Key: s[0], Name: s[1], Index: i + 1, Total: len(plan)})
		if m.StageDelay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.StageDelay):
			}
		}
	}
}

// mockStats 按目标名生成一组**稳定**的运行指标。
//
// 稳定（而不是随机）是刻意的：mock 不维护状态，随机值会让轮询界面上的
// CPU 与 IO 数字自己抖动。那看起来像真实负载在变化，而实际什么都没发生——
// 演示时它会让人以为指标已经接好了，排查时它会浪费掉一轮怀疑。
//
// 取值范围刻意覆盖几种典型形态（低负载、中等、偏高），好让界面上
// 「计量条」的各个档位都能被看到；全都给 3% 会让满格与告警样式永远
// 没被渲染过。
func mockStats(target string) VMStats {
	h := fnv.New32a()
	_, _ = h.Write([]byte("stats/" + target))
	seed := h.Sum32()

	vcpu := float64(seed%75) + 5 // 5% ~ 80%
	used := int(seed%60) + 30    // 30% ~ 90%
	total := 4096

	return VMStats{
		CPUPercent:    math.Round(vcpu*10) / 10,
		MemTotalMB:    total,
		MemUsedMB:     total * used / 100,
		NetRxKbps:     float64(seed%9000)/10 + 12,
		NetTxKbps:     float64((seed/7)%5000)/10 + 8,
		DiskReadKbps:  float64((seed/11)%20000) / 10,
		DiskWriteKbps: float64((seed/13)%12000) / 10,
		// 运行时长取一个稳定但明显是假的值（数小时量级）。
		// 不用真实时钟：那会让「运行时长」每秒都在涨，看起来像真的在跑。
		UptimeSeconds: int64(seed%86400) + 3600,
	}
}

// mockUUID 生成稳定且可辨识的假 UUID。
//
// 用运营者能一眼看出是假的格式（前缀 mock-）：如果它长得像真 UUID，
// 排查问题时很容易把它当成真实虚拟化层的标识。
func mockUUID(nodeID int64, name string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s", nodeID, name)))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("mock-%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// Snapshot 返回固定的运行态：一律「在线」，心跳时间取当前时刻。
//
// 心跳时间用 `time.Now()` 而不是固定值，是为了让界面上的「最后心跳」
// 不至于随着服务运行时间推移显示成几小时前——那看起来像故障。
//
// 注意：这意味着**离线分支在接入真实 agent 前不会被触发**，
// 包括「离线标记」与「离线时不派发任务」等逻辑，需在联调时集中验证
// （见 docs/06-decisions/0007-mock-agent-first.md）。
func (m *MockClient) Snapshot(_ context.Context, _ int64) (*Snapshot, error) {
	now := time.Now()
	return &Snapshot{
		Status:          StatusOnline,
		LastHeartbeat:   now,
		AgentVersion:    "mock-0.1.0",
		ProtocolVersion: 1,
		Capabilities: []string{
			"vm.create", "vm.start", "vm.stop", "vm.delete", "vm.snapshot",
			"storage.pool.create", "network.bridge.list", "network.ovs.configure",
		},
		CapabilitiesAt: now,
	}, nil
}

// OpenStream 返回 ErrStreamUnsupported。
//
// 刻意**不模拟 RFB 握手**：伪造一段能通过握手的字节流会让 noVNC 走到
// 「已连接但永远黑屏」的状态，而那种现象看起来像前端坏了。返回明确的
// 不支持，前端就能给出「通路已建立，当前为模拟模式」这类可理解的提示。
//
// 这属于 ADR-0007 接受的代价：控制台的真实画面需接入真实 agent 后验证。
func (m *MockClient) OpenStream(
	_ context.Context, _ StreamKind, _ int64, _ string,
) (Stream, error) {
	return nil, ErrStreamUnsupported
}

// 编译期断言：MockClient 必须满足 Client 与 StreamOpener 契约。
var (
	_ Client       = (*MockClient)(nil)
	_ StreamOpener = (*MockClient)(nil)
)
