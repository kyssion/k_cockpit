package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"time"
)

// MockClient 是 Client 的假实现。
//
// 它**只保证接口有返回值**，不模拟节点行为：不模拟耗时、进度推进、失败注入
// 与离线（见 docs/06-decisions/0007-mock-agent-first.md）。在仅使用 mock 的阶段，
// 执行失败、超时、节点离线等分支不会被触发，需在联调时集中验证。
//
// 有一个例外：**阶段会上报**。阶段不是「节点行为的模拟」，而是节点向控制面
// 传递信息的一种方式——不上报的话，本项目的任务时间线在此之前
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
//
// 接收整个 Operation 而不只是 Kind：同一个 Kind 下也可能有不同的步骤序列
// （目录共享的挂载与卸载就是如此），而真实节点同样是按参数来决定的——
// 控制面不该替它猜。
func stagePlan(op Operation) [][2]string {
	switch op.Kind {
	case OpPlatformCheck:
		return [][2]string{
			{"read_ovs", "读取 OVS 状态"},
			{"verify_expectations", "逐项核对期望状态"},
		}
	case OpPlatformRepair:
		return [][2]string{
			{"resolve_kinds", "确定要重下发的项"},
			{"reapply", "按期望状态重新下发"},
			{"verify", "复核结果"},
		}
	case OpHostTuning:
		return [][2]string{
			{"read_sysfs", "读取内核状态"},
			{"read_metrics", "读取收益与代价数据"},
		}
	case OpHostTuningApply:
		return [][2]string{
			{"validate", "校验参数"},
			{"write_sysfs", "写入内核参数"},
			{"verify", "确认生效"},
		}
	case OpHostPCIDevices:
		return [][2]string{
			{"scan_pci", "扫描 PCI 设备"},
			{"read_iommu_groups", "读取 IOMMU 分组"},
		}
	case OpHostPCIBind:
		return [][2]string{
			{"check_in_use", "确认设备未被占用"},
			{"rebind_driver", "切换驱动绑定"},
		}
	case OpHostFirewallApply:
		if act, _ := op.Params["action"].(string); act == "rollback" {
			return [][2]string{
				{"remove_our_rules", "撤销本系统写入的规则"},
				{"restore_policy", "恢复 INPUT 默认策略"},
				{"verify_manage_path", "确认管理通道可达"},
			}
		}
		return [][2]string{
			{"render_rules", "渲染规则"},
			{"apply_rules", "写入规则链"},
			{"set_default_policy", "最后才收紧默认策略"},
		}
	case OpQuotaEnforce:
		enforce, _ := op.Params["enforce"].(bool)
		if !enforce {
			return [][2]string{
				{"remove_rules", "移除限速/阻断规则"},
				{"verify_restore", "确认网络恢复"},
			}
		}
		return [][2]string{
			{"resolve_targets", "确定生效范围"},
			{"apply_rule", "施加限流规则"},
			{"verify_rule", "校验规则生效"},
		}
	case OpNetworkCapture:
		return [][2]string{
			{"start_tcpdump", "启动抓包"},
			{"wait_duration", "等待抓包时长"},
			{"flush_file", "落盘并返回文件"},
		}
	case OpPortSecurityApply:
		// 三项保护各对应一组流表：期望状态下发之后，节点上就是这些规则。
		return [][2]string{
			{"render_rules", "渲染流表"},
			{"apply_flows", "写入 OpenFlow 流表"},
			{"verify_flows", "校验流表状态"},
		}
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
	case OpVMNVRAMRepair:
		return [][2]string{
			{"domain_undefine", "解除域定义（保留磁盘）"},
			{"nvram_rebuild", "重建 UEFI 启动项"},
			{"domain_define", "重新定义并校验"},
		}
	case OpHostHardware:
		return [][2]string{
			{"capabilities_read", "读取宿主机能力"},
			{"cpu_topology", "解析 CPU 拓扑"},
			{"memory_scan", "读取内存条信息"},
		}
	case OpHostNetStats:
		return [][2]string{
			{"rules_count", "统计 NAT 与 DNAT 规则"},
			{"switch_counters", "读取交换机计数"},
			{"bridge_counters", "读取网桥计数"},
		}

	case OpTemplateExport:
		return [][2]string{
			{"manifest_build", "生成清单"},
			{"digest_calc", "计算摘要"},
			{"archive_pack", "打包为 tar.gz"},
		}
	case OpTemplateExportDelete:
		return [][2]string{
			{"file_remove", "删除导出包"},
		}
	case OpTemplateImportPreview:
		return [][2]string{
			{"archive_open", "打开模板包"},
			{"manifest_read", "读取清单"},
			{"digest_verify", "校验内容摘要"},
		}
	case OpTemplateImport:
		return [][2]string{
			{"archive_open", "打开模板包"},
			{"digest_verify", "校验内容摘要"},
			{"disk_extract", "解出磁盘镜像"},
			{"disk_convert", "转换磁盘格式"},
			{"template_register", "登记为模板"},
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
	case OpVMReinstall:
		// 顺序体现「先备份、再重建」：先删旧的再重建会让失败时的还原变得
		// 不可能——而重装失败往往正是因为磁盘空间不足或模板有问题，
		// 那恰恰是最不能丢数据的时刻。
		return [][2]string{
			{"disk_backup", "备份原系统盘"},
			{"disk_create", "按模板创建系统盘"},
			{"data_disk_attach", "挂载数据盘"},
			{"domain_update", "更新域配置"},
			{"domain_start", "启动虚拟机"},
		}
	case OpVMReinstallPurge:
		return [][2]string{
			{"backup_remove", "删除备份盘"},
		}
	case OpVMExport:
		return [][2]string{
			{"disk_snapshot_read", "读取磁盘"},
			{"image_convert", "转换镜像格式"},
			{"manifest_write", "生成描述清单"},
			{"export_register", "登记导出产物"},
		}
	case OpVMExportDelete:
		return [][2]string{
			{"export_remove", "删除导出文件"},
		}
	case OpVMMigrate:
		// 两侧用不同前缀分开：迁移有两个独立的失败面，排障时先要分清是
		// 源侧读不出来还是目标侧写不下——混在一条线里会让人从两台机器里猜。
		return [][2]string{
			{"source.stop", "确认源侧已停止"},
			{"source.read", "读取磁盘"},
			{"transfer", "传输数据"},
			{"target.write", "写入目标存储"},
			{"target.define", "在目标节点定义"},
			{"source.cleanup", "清理源侧"},
		}
	case OpHostStats:
		return [][2]string{
			{"read_proc", "读取 /proc 指标"},
			{"read_devices", "读取设备统计"},
		}
	case OpStorageFileCommit:
		return [][2]string{
			{"verify_checksum", "校验文件摘要"},
			{"write_file", "写入用户存储空间"},
			{"fsync_dir", "落盘并同步目录"},
		}
	case OpNetworkProbe:
		return [][2]string{
			{"detect_ovs", "检测 Open vSwitch"},
			{"detect_modules", "检查内核模块"},
			{"list_uplinks", "枚举可用物理口"},
		}
	case OpNetworkUplinkAttach:
		// watch_arm 排在 apply 之前：顺序反了就意味着有一个「已生效、
		// 没兜底」的窗口。
		return [][2]string{
			{"validate_port", "校验物理口"},
			{"watch_arm", "启动自动回滚窗口"},
			{"add_to_bridge", "把物理口加入桥"},
			{"verify_uplink", "确认上行可达"},
		}
	case OpNetworkUplinkConfirm:
		return [][2]string{{"watch_disarm", "取消自动回滚窗口"}}
	case OpNetworkUplinkDetach:
		return [][2]string{
			{"watch_disarm", "取消自动回滚窗口"},
			{"remove_from_bridge", "把物理口移出桥"},
			{"restore_ip", "恢复该口的 IP 配置"},
		}
	case OpNetworkRepair:
		return [][2]string{
			{"inspect_state", "查看当前状态"},
			{"converge", "收敛到期望状态"},
			{"verify_flow", "验证转发"},
		}
	case OpPortMirrorEnable:
		// **看门狗与镜像在同一次调用里建立**，因此阶段里能看到 watch_arm
		// 排在 apply 之前——顺序反了就意味着有一个「已生效、没兜底」的窗口。
		return [][2]string{
			{"validate_topology", "校验接口与目标"},
			{"watch_arm", "启动自动撤销看门狗"},
			{"apply_mirror", "建立镜像"},
			{"verify_flow", "确认流量已复制"},
		}
	case OpPortMirrorConfirm:
		return [][2]string{
			{"watch_disarm", "取消自动撤销看门狗"},
		}
	case OpPortMirrorDisable:
		return [][2]string{
			{"watch_disarm", "取消自动撤销看门狗"},
			{"remove_mirror", "移除镜像"},
		}
	case OpFirewallApply:
		return [][2]string{
			{"render_rules", "渲染规则集"},
			{"backup_chain", "备份现有规则链"},
			{"apply_chain", "写入防火墙链"},
			{"verify_chain", "校验链状态"},
		}
	case OpFirewallRollback:
		// 三步：恢复备份 → 清空本次规则 → 确认不拦截。
		// 最后一步是**验证**而不是记录：回滚的价值在于「确实能进去」，
		// 而不是「命令执行成功了」。
		return [][2]string{
			{"restore_backup", "恢复规则备份"},
			{"flush_rules", "清空本次规则"},
			{"verify_open", "确认已不拦截"},
		}
	case OpStorageVolumeApply:
		action, _ := op.Params["action"].(string)
		if action == "delete" {
			return [][2]string{
				{"lv_remove", "移除逻辑卷"},
				{"vg_remove", "移除卷组"},
				{"pv_release", "释放物理卷"},
			}
		}
		// 顺序体现 LVM 的三层：物理卷 → 卷组 → 逻辑卷。镜像与条带都在
		// 最后一步传参，因此前三步是共用的。
		return [][2]string{
			{"pv_create", "创建物理卷"},
			{"vg_create", "创建卷组"},
			{"lv_create", "创建逻辑卷"},
			{"verify_status", "确认卷状态"},
		}
	case OpShareMount:
		action, _ := op.Params["action"].(string)
		if action == "unmount" {
			return [][2]string{
				{"detach_device", "摘下 virtu1o 设备"},
				{"release_dir", "释放目录访问"},
			}
		}
		return [][2]string{
			{"validate_path", "校验共享路径"},
			{"attach_device", "挂入 virtu1o 设备"},
			{"propagate", "等待来宾识别"},
		}
	case OpSecurityGroupApply:
		return [][2]string{
			{"render_rules", "渲染规则"},
			{"apply_chain", "写入规则链"},
			{"verify_chain", "校验链状态"},
		}
	case OpPublicIPChange:
		// 顺序体现「先撤旧、再加新」——迁移中途失败时旧规则仍然有效，
		// 地址不会悬空。
		return [][2]string{
			{"validate_addr", "校验收敛地址状态"},
			{"flush_old", "移除旧规则"},
			{"apply_new", "应用新规则"},
			{"verify_reach", "验证可达性"},
		}
	case OpImageImport:
		return [][2]string{
			{"source_verify", "校验源文件"},
			{"format_convert", "转换格式"},
			{"disk_place", "放入存储池"},
			{"template_register", "登记为模板"},
		}
	case OpVMDiskChange:
		// 两步：先在宿主机侧改设备，再确认来宾是否需要重启。第二步不是
		// "多余的一步"——换总线之后来宾里的设备路径会变，那是唯一需要
		// 明确告知用户的事。
		return [][2]string{
			{"device_apply", "应用磁盘配置"},
			{"guest_check", "检查来宾是否需要重启"},
		}
	case OpVMCDROMApply:
		return [][2]string{
			{"render_device", "渲染光驱设备"},
			{"apply_domain", "写入域配置"},
			{"verify_media", "确认介质状态"},
		}
	case OpVMDisksIndependent:
		return [][2]string{
			{"verify_stopped", "确认虚拟机已停机"},
			{"merge_backing", "合并底层镜像"},
			{"verify_standalone", "确认不再依赖父盘"},
		}
	case OpVMDiskResize:
		return [][2]string{
			{"check_shrinking", "确认是扩容而非缩容"},
			{"grow_image", "扩大镜像文件"},
			{"update_domain", "更新域定义"},
		}
	case OpVMXML:
		return [][2]string{
			{"dumpxml", "导出域定义"},
		}
	case OpVMGuest:
		// 阶段按 `host.` / `guest.` 前缀分成两段。
		//
		// 这个分界必须在时间线上体现出来：宿主机阶段失败通常是权限、路径、
		// 设备占用；来宾阶段失败通常是系统没起来、agent 没装、密码策略拒绝。
		// 两者的排查方向完全不同，混在一条线里会让人找错方向。
		return [][2]string{
			{"host.prepare", "准备宿主机环境"},
			{"host.attach", "挂载磁盘"},
			{"guest.connect", "连接 Guest Agent"},
			{"guest.execute", "在来宾内执行"},
			{"host.detach", "卸载并清理"},
		}
	}
	// 电源类操作是单步的，但仍然会上报一项：否则界面上这类任务的时间线是
	// 空的，而「空空如也」与「不支持展示」看起来是同一件事。
	switch op.Kind {
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

	case OpVMReinstall:
		data[ReinstallDataKey] = ReinstallInfo{
			BackupPath: "/var/lib/k_cockpit/backups/" + op.Target + ".qcow2.bak",
			DiskPath:   "/var/lib/k_cockpit/images/" + op.Target + ".qcow2",
		}
		data["uuid"] = mockUUID(op.NodeID, op.Target)

	case OpVMReinstallPurge:
		// 没有返回值：清理的结果只由 Success 表达。

	case OpVMExport:
		ext := "qcow2"
		if f, ok := op.Params["format"].(string); ok && f != "" {
			ext = f
		}
		data[ExportDataKey] = ExportInfo{
			FilePath: "/var/lib/k_cockpit/exports/" + op.Target + "." + ext,
			FileName: op.Target + "." + ext,
			// 大小刻意给一个**非整值**：整数 GB 会让格式化逻辑里
			// 「不足 1 GB 显示 MB」那条分支永远走不到。
			SizeBytes: int64(3_421_000_000),
		}

	case OpVMExportDelete:
		// 没有返回值：删除的结果只由 Success 表达。

	case OpVMMigrate:
		data[MigrateResultKey] = MigrateResult{
			// 说明跟着搬了些什么：只给一个「成功」会让用户不确定
			// 「我原来接的网络、配的转发还在不在」。
			Moved:           []string{"系统盘与数据盘", "全部网卡", "静态地址与端口转发"},
			DurationSeconds: 47,
		}

	case OpHostStats:
		// 稳定值：mock 不维护状态，随机值会让曲线自己抖动——那看起来像
		// 真实负载在变化，而实际什么都没发生。
		// 设备明细：两块网卡 + 两块磁盘。
		//
		// **必须给全**：界面上的「按设备筛选」下拉与分设备曲线都靠它。给空
		// 的话那条路径永远走不到，而它恰恰是"整体流量涨了，是哪一块涨的"
		// 唯一能回答问题的入口。
		devices, _ := json.Marshal([]HostDeviceStat{
			{Name: "eth0", Kind: "net", ReadBytes: 9_000_000, WriteBytes: 6_000_000},
			{Name: "eth1", Kind: "net", ReadBytes: 3_300_000, WriteBytes: 2_700_000},
			{Name: "sda", Kind: "disk", ReadBytes: 30_000_000, WriteBytes: 15_000_000},
			{Name: "sdb", Kind: "disk", ReadBytes: 15_700_000, WriteBytes: 8_400_000},
		})

		data[HostStatsDataKey] = HostStats{
			CPUPercent: 23.5, CPUCores: 8, MemUsedMB: 6144, MemTotalMB: 16384, SwapUsedMB: 0,
			Devices: string(devices),
			Load1:   0.8, Load5: 0.6, Load15: 0.5,
			NetInBytes: 12_345_678, NetOutBytes: 8_765_432,
			DiskReadBytes: 45_678_901, DiskWriteBytes: 23_456_789,
			UptimeSeconds: 864_000,
		}

	case OpStorageFileCommit:
		// 回显控制面给的摘要与大小：契约要求两者一致，不一致时调用方会把
		// 这次上传判为失败。mock 里如实回显，让那条分支不被误触发。
		rel, _ := op.Params["rel_path"].(string)
		size, _ := op.Params["size"].(int64)
		sum, _ := op.Params["checksum"].(string)
		data[StorageFileDataKey] = StorageFileInfo{
			RelPath: rel, SizeBytes: size, Checksum: sum,
			Message: "文件已写入用户存储空间",
		}

	case OpNetworkProbe:
		data[NetworkCapabilityKey] = NetworkCapability{
			OVSAvailable:  false,
			KernelModules: []string{"br_netfilter", "vhost_net"},
			UplinkCandidates: []UplinkCandidate{
				{Name: "eth0", Up: true, HasIP: true, Speed: "1000Mb/s"},
				{Name: "eth1", Up: true, HasIP: false, Speed: "10000Mb/s"},
				{Name: "eth2", Up: false, HasIP: false},
			},
			Notes: []string{"未检测到 Open vSwitch，已降级到 Linux 网桥"},
		}

	case OpNetworkBridgeApply:
		data[NetworkRepairKey] = NetworkRepairInfo{Message: "网络已建立"}

	case OpNetworkBridgeDelete:
		data[NetworkRepairKey] = NetworkRepairInfo{Message: "网络已删除"}

	case OpNetworkUplinkAttach:
		seconds, _ := op.Params["watchdog_seconds"].(int)
		data[NetworkRepairKey] = NetworkRepairInfo{
			Message: "物理口已入桥，自动回滚窗口 " + strconv.Itoa(seconds) + " 秒",
		}

	case OpNetworkUplinkConfirm:
		data[NetworkRepairKey] = NetworkRepairInfo{Message: "已确认保持"}

	case OpNetworkUplinkDetach:
		data[NetworkRepairKey] = NetworkRepairInfo{Message: "物理口已摘出"}

	case OpNetworkRepair:
		data[NetworkRepairKey] = NetworkRepairInfo{
			Fixed:     []string{"默认网络的 DHCP 服务已重新拉起"},
			Remaining: []string{"节点未安装 Open vSwitch，增强模式仍不可用"},
			Message:   "修复已完成，仍有 1 项需要人工处理",
		}

	case OpPortMirrorEnable:
		seconds, _ := op.Params["watchdog_seconds"].(int)
		data[PortMirrorDataKey] = PortMirrorInfo{
			WatchdogSeconds: seconds,
			Applied:         1,
			Message:         "镜像已建立；看门狗已同时在节点侧启动",
			Warnings: []string{
				"来源接口镜像到目标交换机的流量会翻倍占用带宽",
			},
		}

	case OpPortMirrorConfirm:
		data[PortMirrorDataKey] = PortMirrorInfo{
			Message: "看门狗已取消，镜像将保持生效",
		}

	case OpPortMirrorDisable:
		data[PortMirrorDataKey] = PortMirrorInfo{
			Message: "镜像已撤销",
		}

	case OpFirewallApply:
		n := 0
		if list, ok := op.Params["rules"].([]map[string]any); ok {
			n = len(list)
		}
		data[FirewallDataKey] = FirewallInfo{
			Applied: n,
			Message: "防火墙规则已写入宿主机链",
		}

	case OpFirewallRollback:
		// 回滚**不应该失败**：一个恢复入口如果自己会失败，它就不是恢复入口。
		data[FirewallDataKey] = FirewallInfo{
			Message: "已撤销本次下发，防火墙恢复为不拦截",
		}

	case OpStorageVolumeApply:
		action, _ := op.Params["action"].(string)
		if action == "delete" {
			data[VolumeDataKey] = VolumeInfo{Message: "存储卷已删除，设备已释放"}
			break
		}
		mirror, _ := op.Params["mirror_count"].(int)
		size, _ := op.Params["size_gb"].(int)
		info := VolumeInfo{SizeGB: size, PhysicalGB: size, Status: "active",
			Message: "存储卷已创建"}
		if mirror > 1 {
			info.PhysicalGB = size * mirror
			// 镜像卷建好之后要同步几小时，期间写明显变慢——节点如实回报
			// 「同步中」，而不是一律说 active。
			info.Status = "sync"
			info.Message = "存储卷已创建，镜像正在初始同步"
			info.Warnings = []string{"同步期间写性能会明显下降，完成后自动转为正常"}
		}
		data[VolumeDataKey] = info

	case OpShareMount:
		action, _ := op.Params["action"].(string)
		msg := "共享目录已挂载"
		if action == "unmount" {
			msg = "共享已卸载"
		}
		tag, _ := op.Params["tag"].(string)
		info := ShareInfo{Tag: tag, Message: msg}
		// 只读共享下提示一次：这是用户最容易忽略、而后果最实际的一项。
		if ro, _ := op.Params["read_only"].(bool); ro && action != "unmount" {
			info.Warnings = []string{"该共享以只读方式挂载，来宾内无法写入"}
		}
		data[ShareDataKey] = info

	case OpSecurityGroupApply:
		n := 0
		if list, ok := op.Params["rules"].([]map[string]any); ok {
			n = len(list)
		}
		data[SecurityGroupDataKey] = SecurityGroupInfo{
			Applied: n,
			Message: "规则已写入运行域",
		}

	case OpPublicIPChange:
		action, _ := op.Params["action"].(string)
		msg := "地址已绑定"
		switch action {
		case "unbind":
			msg = "地址已解绑"
		case "migrate":
			msg = "地址已迁移到新虚拟机"
		}
		data[PublicIPDataKey] = PublicIPInfo{Message: msg}

	case OpPublicIPPreview:
		// 预览要给**具体的规则文本**，而不是一句「将会新增若干规则」：
		// f-4-06 要求预览的全部意义就是让用户看清将要发生什么，
		// 一句概括等于什么都没说。
		mode, _ := op.Params["mode"].(string)
		vmName, _ := op.Params["vm_name"].(string)
		preview := PublicIPPreview{
			Added: []string{
				"iptables -t nat -A PREROUTING -d " + op.Target + " -j DNAT --to-destination 192.168.122.10",
				"iptables -t nat -A POSTROUTING -s 192.168.122.10 -j SNAT --to-source " + op.Target,
			},
		}
		if mode == "routed" {
			preview.Added = []string{
				"ip route replace " + op.Target + "/32 dev kbr0",
				"arp -s " + op.Target + " <guest-mac>",
			}
		}
		if vmName != "" {
			preview.Added = append(preview.Added, "# 目标虚拟机："+vmName)
		}
		preview.Warnings = []string{
			"该地址在宿主机上已有一条手工添加的路由，应用后将以控制面的规则为准",
		}
		data[PublicIPPreviewKey] = preview

	case OpImageParse:
		// 模拟从 OVA 内的 OVF 描述解析出的配置。
		//
		// 给一组**非默认**的值（4 核 / 8 GB）：mock 若返回默认值，界面与
		// 流程里「用解析出来的值覆盖默认值」那段逻辑就永远走不到——而它
		// 正是「先解析预览」这个功能存在的意义。
		data[ImageParseDataKey] = ImagePreview{
			VCPU: 4, MemoryMB: 8192, DiskGB: 40,
			OSType: "linux", OSVariant: "ubuntu24.04",
			Sources: map[string]string{"vcpu": "ovf", "memory_mb": "ovf", "disk_gb": "ovf"},
		}

	case OpImageImport:
		format, _ := op.Params["source_format"].(string)
		var notes []string
		if format != "" && format != "qcow2" {
			notes = append(notes, "源格式 "+format+" 已转换为 qcow2")
		}
		data[ImageImportDataKey] = ImageImportInfo{
			DiskPath: "/var/lib/k_cockpit/imports/" + op.Target + ".qcow2",
			SizeGB:   40,
			Format:   "qcow2",
			Notes:    notes,
		}

	case OpVMXML:
		name, _ := op.Params["domain_name"].(string)
		if name == "" {
			name = op.Target
		}
		live, _ := op.Params["live"].(bool)
		// **定义里带一处密码**：不脱敏的话，界面特意「只写不读」的控制台
		// 密码会从这个后门原样送出去。
		data[VMXMLKey] = VMXMLInfo{
			Live: live,
			XML: `<domain type='kvm'>
  <name>` + name + `</name>
  <memory unit='MiB'>2048</memory>
  <vcpu>2</vcpu>
  <devices>
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1' passwd='s3cret'/>
    <disk type='file' device='disk'>
      <source file='/var/lib/libvirt/images/` + name + `.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
  </devices>
</domain>`,
		}

	case OpVMCDROMApply:
		action, _ := op.Params["action"].(string)
		info := CDROMInfo{Message: "光驱配置已更新"}
		switch action {
		case "eject":
			info.Message = "已弹出光盘（光驱仍在）"
		case "remove":
			info.Message = "已移除光驱"
		case "load":
			// **换盘之后来宾通常看不到新介质**：多数系统缓存了介质信息。
			// 刻意让这条提示可达，界面就必须显示它——否则用户会以为换盘
			// 失败而反复重试。
			info.Message = "已更换光盘"
			info.GuestRefreshNeeded = true
		case "bus":
			info.Message = "已更换总线类型"
			info.RebootNeeded = true
		}
		data[CDROMDataKey] = info

	case OpVMDisksIndependent:
		freed, _ := op.Params["freed_from"].(string)
		data[VMDiskIndependentKey] = VMDiskIndependentInfo{
			Applied: true, FreedFrom: freed, SizeBytes: 8 << 30,
			Message: "磁盘已合并为独立镜像",
		}

	case OpVMDelete:
		// 只有 disk_action=transfer 时才有可回传的东西：其余两种情况
		// （保留 / 连盘删除）节点上不会留下任何"文件"，返回空是准确的。
		if strParam(op.Params, "disk_action") == DiskActionTransfer {
			data[VMDiskTransferDataKey] = []TransferredDisk{{
				Dev:       "vdb",
				Filename:  op.Target + "-data.img",
				RelPath:   "disks/" + op.Target + "-data.img",
				SizeBytes: 22 << 30,
			}}
		}

	case OpVMDiskResize:
		oldGB, _ := op.Params["old_gb"].(int)
		newGB, _ := op.Params["new_gb"].(int)
		data[VMDiskDataKey] = VMDiskResizeInfo{
			Applied: true, OldGB: oldGB, NewGB: newGB,
			// **刻意标出"来宾里还要扩"**：这是这个操作最容易让用户误解的
			// 一点——宿主机侧扩完只是"盘子变大了"，而操作系统看到的仍是
			// 原来的分区。让这条分支可达，界面就必须把它说清楚。
			GuestGrowNeeded: true,
			GuestGrowHint: "在来宾里用 growpart /dev/vda 1 扩分区，再 resize2fs /dev/vda1 扩文件系统" +
				"（Windows 用「磁盘管理」的「扩展卷」）",
			Message: "宿主机侧已扩容",
		}

	case OpVMDiskList:
		// 两块盘：系统盘（virtio / qcow2，不可卸载）+ 一块数据盘（scsi / raw）。
		//
		// 形状必须**完整且稳定**：界面上每一列都靠它渲染，少一个字段的表现
		// 是那一列空白，而空白会被读成"没数据"而不是"mock 没给"。
		data[VMDiskListDataKey] = []VMDisk{
			{
				Dev: "vda", CapacityGB: 40, ActualBytes: 8_500_000_000,
				Format: "qcow2", Bus: "virtio",
				Source:       "/var/lib/k_cockpit/images/" + op.Target + ".qcow2",
				IsSystem:     true,
				Hotpluggable: false,
			},
			{
				Dev: "vdb", CapacityGB: 100, ActualBytes: 22_000_000_000,
				Format: "raw", Bus: "scsi",
				Source:       "/var/lib/k_cockpit/images/" + op.Target + "-data.img",
				IsSystem:     false,
				Hotpluggable: true,
			},
		}

	case OpVMDiskChange:
		// 换总线要重启、挂载后来宾要重新扫描：这两条提示由**动作本身**决定
		// 而不是随机，界面要按它们给出不同的后续指引。
		info := VMDiskChangeInfo{Dev: strParam(op.Params, "dev")}
		switch strParam(op.Params, "action") {
		case DiskActionBus:
			info.RebootNeeded = true
			info.Message = "总线类型已变更，需重启虚拟机后生效"
		case DiskActionAttach:
			info.Dev = "vdc"
			info.GuestRefreshNeeded = true
			info.Message = "磁盘已挂载为 vdc，来宾内可能需要重新扫描或挂载"
		case DiskActionDetach:
			info.GuestRefreshNeeded = true
			info.Message = "磁盘已卸载；来宾内若有对应挂载点，请先卸载再操作"
		case DiskActionMigrate:
			// 热迁移不需要重启，但来宾里可能要重新扫描——与挂载同理，
			// 这两条提示由动作本身决定，界面据此给出不同的后续指引。
			info.GuestRefreshNeeded = true
			info.Message = "磁盘已迁移到目标存储，来宾内可能需要重新扫描"
		default:
			info.Message = "磁盘配置已更新"
		}
		data[VMDiskDataKey] = info

	case OpVMXMLApply:
		action, _ := op.Params["action"].(string)
		if action == "validate" {
			xml, _ := op.Params["xml"].(string)
			info := VMXMLValidateInfo{Valid: true}
			// **刻意留一条能失败的路径**：缺少 <domain> 根元素的定义会被
			// libvirt 拒绝，而界面上必须能渲染出那个错误。只给"永远校验通过"
			// 的假数据，这条分支永远不会被走到。
			if !strings.Contains(xml, "<domain") {
				info.Valid = false
				info.Errors = []string{"这份定义缺少 <domain> 根元素，libvirt 拒绝接受"}
			}
			data[VMXMLValidateKey] = info
			break
		}
		data[VMXMLValidateKey] = VMXMLValidateInfo{
			Valid:    true,
			Warnings: []string{"该改动可能需要在虚拟机重启后才会完全生效"},
		}

	case OpVMGuest:
		action, _ := op.Params["action"].(string)
		data[GuestDataKey] = GuestInfo{
			// 用 agent 的动作：在线改密、附加磁盘、扩容；离线改密不用。
			GuestAgentUsed: action != "password_offline",
			Message:        guestMessage(action),
		}

	case OpVMExportFetch:
		// 返回一段**一眼能看出是占位**的内容，而不是伪造一个像真的镜像。
		//
		// 与控制台截帧同一个理由：一份看起来像真实 OVA 的字节流会让人以为
		// 导出通路已经打通了，从而不去验证真实链路（大文件分块、断点续传、
		// 校验）。占位内容不会造成这种误解。
		data[ExportContentKey] = ExportContent{
			Data: []byte("k_cockpit mock export placeholder\n" +
				"节点侧会由节点流式返回导出产物。\n" +
				"target=" + op.Target + "\n"),
			MIME: "application/octet-stream",
		}
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

	case OpNetworkCapture:
		iface, _ := op.Params["interface"].(string)
		filter, _ := op.Params["filter"].(string)
		dur, _ := op.Params["duration_sec"].(int)
		info := CaptureInfo{
			FilePath: "/var/lib/k_cockpit/captures/" + iface + ".pcap",
			// 大小与时长成正比，让界面上的格式化与"文件多大"有真实感。
			SizeBytes: int64(dur) * 1024 * 12,
			Message:   "抓包完成",
		}
		if filter != "" && !strings.Contains(filter, "port") {
			// 一个**抓不到东西**的结果：过滤器没匹配到流量。
			//
			// 刻意让这条分支可达：空文件是一个看不出原因的结果，而用户会
			// 先去怀疑抓包功能坏了。界面必须把"过滤器可能有问题"说出来。
			info.SizeBytes = 0
			info.Message = "抓包完成，但没有匹配到任何流量"
			info.Warnings = []string{"过滤器可能过于严格，可以先用空过滤器确认该网口上有没有流量"}
		}
		data[CaptureDataKey] = info

	case OpNetworkCaptureDelete:
		data[CaptureDataKey] = CaptureInfo{Message: "抓包文件已删除"}

	case OpOVSStatus:
		data[OVSStatusKey] = OVSStatus{
			Available: true, Version: "2.17.9", ServiceActive: true,
			OpenFlow13: true,
			// meter 缺失：包速率限制依赖它，而这条分支必须可达——否则
			// 「能力非必需但缺失」在自检里永远不会出现。
			MeterAvailable: false,
			BridgeCount:    2, PortCount: 7, FlowCount: 43,
			Fix: "apt install openvswitch-common   # 需要支持 meter 的版本",
		}

	case OpOVSPorts:
		data[OVSPortsKey] = []OVSPort{
			{Name: "br-tenant0", Bridge: "br-tenant0", Type: "internal", Tag: 0},
			// 虚拟机网口：VMName 由**节点**填（只有它能从 libvirt 拿到
			// vnet 口与虚拟机的对应关系）。
			{Name: "vnet0", Bridge: "br-tenant0", Type: "system", Tag: 100, VMName: "vm-101"},
			{Name: "vnet1", Bridge: "br-tenant0", Type: "system", Tag: 100, VMName: "vm-102"},
			{Name: "eth1", Bridge: "br-tenant0", Type: "system", Tag: 0},
		}

	case OpDHCPLeases:
		data[DHCPLeasesKey] = []DHCPLease{
			{ExpiresAt: "2026-09-20T10:00:00Z", MAC: "52:54:00:aa:bb:01",
				IP: "192.168.122.10", Hostname: "vm-101", ClientID: "01:52:54:00:aa:bb:01"},
			{ExpiresAt: "2026-09-20T10:00:00Z", MAC: "52:54:00:aa:bb:02",
				IP: "192.168.122.11", Hostname: "vm-102", ClientID: "01:52:54:00:aa:bb:02"},
			// 一条**没有主机名**的租约：多台机器抢同一个地址时它是最先
			// 需要看到的那条，而"主机名"这一栏空着正是它的特征。
			{ExpiresAt: "2026-09-20T09:30:00Z", MAC: "52:54:00:cc:dd:03",
				IP: "192.168.122.10"},
		}

	case OpPlatformCheck:
		// 逐项回报：**一部分存在、一部分是偏差**。
		//
		// 全都报"存在"会让偏差分支永远不可达，而那个分支是这整个功能的
		// 意义所在——面板显示"已启用"而节点上早就没了。
		// **期望是类型化结构体，而不是 JSON 反序列化后的 map。**
		//
		// 真实节点收到的是 JSON（Object → map），而进程内的 mock 拿到的是
		// 控制面直接传过来的 []any of struct。只处理 map 的话，ID 全部读成
		// 0、Kind 全部读成空串——而自检结果仍然会返回一批"项"，只是对不上
		// 号。那是**看起来正常、实际全错**的一类问题。
		exps, _ := op.Params["expectations"].([]any)
		results := make([]PlatformCheckResult, 0, len(exps))
		for i, it := range exps {
			id := int64(0)
			switch v := it.(type) {
			case PlatformExpectation:
				id = v.ID
			case map[string]any:
				switch x := v["ID"].(type) {
				case float64:
					id = int64(x)
				case int64:
					id = x
				}
			}
			// 第 2 项起报偏差：让两种结果都出现在同一次自检里——
			// 只报"全部正常"会让偏差分支永远不可达，而那是这个功能的全部意义。
			present := i%2 == 0
			actual := "存在"
			if !present {
				actual = "无对应配置（可能已被重启清除或手工改动）"
			}
			results = append(results, PlatformCheckResult{ID: id, Present: present, Actual: actual})
		}
		data[PlatformCheckKey] = results

	case OpPlatformRepair:
		data[HostFirewallDataKey] = PCIBindInfo{Message: "已按控制面记录重新下发"}

	case OpHostTuning:
		// KSM **开着但几乎没省下东西**——刻意让这条分支可达。
		//
		// 它是最容易被用户忽略、也最不该被忽略的一种状态：开关显示"已启用"，
		// 而实际上这台机器上根本没有可合并的页，它只是在白耗 CPU。只给
		// "省了 8GB" 的漂亮数据，用户就永远学不会去看扫描轮次与收益。
		data[TuningStateKey] = TuningState{
			KSM: KSMState{
				Enabled: true, PagesShared: 42, PagesSharing: 55,
				PagesUnshared: 1_200_000, SavedBytes: 53 * 4096,
				FullScans: 1841, RunMode: "always",
			},
			ZRAM: ZRAMState{
				Enabled: true, DisksizeBytes: 4 << 30,
				UsedBytes: 512 << 20, OrigDataBytes: 2 << 30,
				Algorithm: "lz4", MemLimitBytes: 2 << 30,
			},
			// 嵌套虚拟化开着但**不是持久的**：重启就没了。这条分支同样要可达
			// ——用户在界面上看到"已启用"，重启之后又变回去，而他会以为是
			// 面板没保存成功。
			Nested: NestedState{
				Enabled: true, Supported: true, Persistent: false,
				Fix: "写入 /etc/modprobe.d/kvm.conf：options kvm_intel nested=1",
			},
		}

	case OpHostTuningApply:
		item, _ := op.Params["item"].(string)
		data[HostFirewallDataKey] = PCIBindInfo{Message: "调优项 " + item + " 已下发"}

	case OpHostPCIDevices:
		// 三块设备，含**一个两块卡同组**的分组：只给"每块卡各自一组"的假
		// 数据会让「同组只能给一台机器」这条分支永远不可达——而它恰恰是
		// 用户最先撞上的那堵墙。
		data[PCIDevicesKey] = []PCIDevice{
			{Address: "0000:01:00.0", VendorDevice: "10de:1eb8", Class: "VGA",
				Description: "NVIDIA Corporation GP104 [GeForce GTX 1080]",
				IOMUGroup:   1, CanPassthrough: true},
			{Address: "0000:01:00.1", VendorDevice: "10de:10f0", Class: "Audio",
				Description: "NVIDIA Corporation GP104 High Definition Audio",
				IOMUGroup:   1, CanPassthrough: true},
			{Address: "0000:00:1f.6", VendorDevice: "8086:15b8", Class: "Network",
				Description: "Intel Corporation Ethernet Connection (2) I219-V",
				Driver:      "e1000e", IOMUGroup: 2,
				Reason: "该设备与宿主机管理网络共用，直通会让面板失去连接"},
			{Address: "0000:03:00.0", VendorDevice: "144d:a808", Class: "Storage",
				Description: "Samsung NVMe SSD", Driver: "nvme", IOMUGroup: 3,
				CanPassthrough: true},
		}

	case OpHostIOMMU:
		data[IOMMUStatusKey] = IOMMUStatus{Enabled: true, VFIOAvailable: true}

	case OpHostPCIBind:
		if attach, _ := op.Params["attach"].(bool); attach {
			data[HostFirewallDataKey] = PCIBindInfo{Message: "设备已挂载到虚拟机"}
		} else {
			data[HostFirewallDataKey] = PCIBindInfo{Message: "设备已从虚拟机卸载"}
		}

	case OpHostFirewallApply:
		act, _ := op.Params["action"].(string)
		msg := "宿主机防火墙已应用"
		rules := 0
		if list, ok := op.Params["rules"].([]any); ok {
			rules = len(list)
		}
		if act == "rollback" {
			msg = "宿主机防火墙已回滚：本系统写入的规则全部撤销"
		}
		info := HostFirewallInfo{AppliedRules: rules, Message: msg}
		// 白名单为空 + 默认拒绝：一个**会把人锁在门外**的组合。刻意让这条
		// 警告可达，好让界面必须处理它。
		if auto, _ := op.Params["default_action"].(string); auto == "deny" {
			if wl, ok := op.Params["whitelist"].([]any); ok && len(wl) == 0 {
				info.Warnings = []string{"白名单为空且默认拒绝——应用后将无法从任何地址连上这台机器"}
			}
		}
		data[HostFirewallDataKey] = info

	case OpHostConnections:
		if target, ok := op.Params["close"].(string); ok && target != "" {
			data[HostFirewallDataKey] = HostFirewallInfo{Message: "连接 " + target + " 已关闭"}
			break
		}
		// 三条有代表性的连接：面板、SSH、外部来源。只给一条会让
		// 「关掉自己那条」的分支永远不可达。
		data[HostConnectionsKey] = []HostConnection{
			{RemoteAddr: "203.0.113.9:51234", LocalPort: 8080, Protocol: "tcp",
				State: "ESTABLISHED", Process: "k-cockpit"},
			{RemoteAddr: "203.0.113.9:51235", LocalPort: 22, Protocol: "tcp",
				State: "ESTABLISHED", Process: "sshd"},
			{RemoteAddr: "198.51.100.7:40000", LocalPort: 9090, Protocol: "tcp",
				State: "TIME_WAIT", Process: "node-exporter"},
		}

	case OpQuotaEnforce:
		enforce, _ := op.Params["enforce"].(bool)
		action, _ := op.Params["action"].(string)
		msg := "配额处置已生效"
		if !enforce {
			msg = "配额处置已撤销"
		} else if action == "block" {
			msg = "已按配额断开该用户的网络"
		} else {
			msg = "已按配额限制该用户的带宽"
		}
		data[QuotaDataKey] = QuotaInfo{Applied: true, Affected: 1, Message: msg}

	case OpPortSecurityPrecheck:
		// **这里报告 OVS 可用，而 OpNetworkProbe / OpNodeNetwork 报告它缺失。**
		//
		// 这个矛盾是刻意留下的，选的是两边各要什么：
		//
		//   - 网络页面需要演示**降级**：能力缺失时该显示什么、给出哪个安装
		//     命令。那条分支只能靠"缺"来走到。
		//   - 端口安全与端口镜像**以 OVS 为前提**。若这里一并报缺失，这两
		//     块功能在演示环境里就完全不可用——用户看不到它们长什么样、会
		//     下发什么规则，而"根本没法配"与"配了不生效"是两种不同的坏，
		//     后者更危险，但前者让整块功能在界面上不存在。
		//
		// 真实节点上两者必然一致（都来自同一次探测）。要消掉这个矛盾，
		// 应当给 mock 引入**多节点场景**（一台装了 OVS、一台没装），而不是
		// 让某一处继续将就。这一条记在这里，免得后来的人以为是笔误。
		spoof, _ := op.Params["spoofing_guard"].(bool)
		iso, _ := op.Params["isolation"].(bool)
		pps, _ := op.Params["pps_limit"].(int)

		rules := []string{"# 端口 " + op.Target + " 的端口安全规则"}
		if spoof {
			rules = append(rules,
				"priority=200,ip,dl_src=<guest-mac>,nw_src=<guest-ip> actions=normal",
				"priority=100,ip actions=drop   # 源地址不属于本网口")
		}
		if iso {
			rules = append(rules,
				"priority=150,ip,nw_src=<guest-ip> actions=drop   # 端口隔离")
		}
		if pps > 0 {
			rules = append(rules,
				"meter=<m0> pktps="+strconv.Itoa(pps)+",burst   # 包速率限制")
		}
		data[PortSecurityPrecheckKey] = PortSecurityPrecheck{
			Capabilities: []PortSecurityCapability{
				{Key: "network.ovs", Label: "Open vSwitch", Required: true},
				{Key: "network.ovs.openflow13", Label: "OpenFlow 1.3", Required: true},
				{
					Key: "network.ovs.meter", Label: "OVS 包速率 meter",
					// **非必需**：缺它只失去限速能力，防伪造与隔离照常工作。
					// 标成必需会让一个没装 meter 的节点连防伪造都用不了。
					Required: pps > 0,
					Missing:  pps > 0,
					Reason:   "未检测到 OVS meter",
					Fix:      "apt install openvswitch-common   # 需要支持 meter 的版本",
				},
			},
			Rules: rules,
			Warnings: []string{
				"该网口上已有 2 条手工添加的流表，应用后将以控制面的规则为准",
			},
		}

	case OpPortSecurityApply:
		data[PortSecurityDataKey] = PortSecurityInfo{
			Applied: true,
			Message: "端口安全规则已写入节点流表",
		}

	case OpStoragePoolScan:
		// 池内是否存在磁盘——这是**删除路径上唯一的保护**（R-008）。
		//
		// 一律返回空会让这条检查永远不被走到；而一律返回非空更糟：那样
		// 任何池都删不掉，连正常流程都走不通。按设备路径区分，两条分支
		// 才都真实可达。
		//
		// 判据与 OpNodeDisks 的假数据对齐：sdc 就是那块标了「已含数据」
		// 的盘，因此在它上面建的池里确实有东西。演示时会看到：建在 sdb
		// （空闲盘）上的池能删掉，建在 sdc 上的会被拦下并列出占用者——
		// 那正是这条检查存在的意义。
		volumes := []string{}
		if strings.Contains(op.Target, "sdc") {
			volumes = []string{"vm-101-disk0", "vm-102-disk0"}
		}
		data[VolumeListKey] = volumes

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

	case OpNodeStats:
		data[NodeStatsDataKey] = mockNodeStats(op.NodeID)

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
		// 不在这里自己生成：节点会把它实际使用的名字返回，
		// 而控制面必须能处理「返回的名字与请求的不同」这一情况。
		if name, ok := op.Params["domain_name"].(string); ok {
			data["domain_name"] = name
		}
		// 给一个非零体积：默认 0 会让界面上的「0 B」看起来像没创建成功，
		// 而这个模拟值正好用来验证体积的展示与格式化。
		data["size_bytes"] = float64(256 * 1024 * 1024)

	case OpVMNVRAMRepair:
		// 回传重建后的启动项：界面要能显示"现在从哪启动"，否则用户只能
		// 靠开机试一次来验证。
		data[NVRAMDataKey] = NVRAMRepairInfo{
			BootEntry: "Boot0001",
			BootPath:  `\EFI\BOOT\BOOTX64.EFI`,
			Message:   "已重建 UEFI 启动项，请重新启动虚拟机确认",
		}

	case OpTemplateExportFetch:
		// 与虚拟机导出同一个理由：返回一眼能看出是占位的内容，而不是伪造
		// 一个像真的 tar.gz。看起来像真包会让人以为链路已经打通了。
		data[ExportContentKey] = ExportContent{
			Data: []byte("k_cockpit mock template export placeholder\n" +
				"节点侧会由节点流式返回导出包。\n" +
				"target=" + op.Target + "\n"),
			MIME: "application/gzip",
		}

	case OpHostHardware:
		// 每核占用给一组不同的值：全 0 或全相同的曲线在界面上看起来
		// 像"数据没采到"，而实际上这正是要展示的东西。
		data[HostHardwareDataKey] = HostHardware{
			CPUModel:       "Mock CPU",
			Sockets:        2,
			CoresPerSocket: 8,
			ThreadsPerCore: 2,
			CorePercent:    []float64{12, 30, 8, 44, 21, 17, 60, 5, 33, 27, 9, 14, 41, 22, 18, 36, 25, 11, 47, 13, 29, 6, 38, 20, 16, 34, 24, 10, 51, 15, 28, 7},
			MemSlots: []MemSlot{
				{Index: 1, SizeMB: 16384, Populated: true, Label: "DIMM_A1"},
				{Index: 2, SizeMB: 16384, Populated: true, Label: "DIMM_A2"},
				{Index: 3, SizeMB: 0, Populated: false, Label: "DIMM_B1"},
				{Index: 4, SizeMB: 0, Populated: false, Label: "DIMM_B2"},
			},
		}

	case OpHostNetStats:
		data[HostNetStatsDataKey] = HostNetStats{
			NATRules:           12,
			SwitchIngressBytes: 48 << 30,
			SwitchEgressBytes:  31 << 30,
			DNATRules:          7,
			Bridges: []BridgeStat{
				{Name: "br-lan", RxBytes: 22 << 30, TxBytes: 18 << 30, RxPackets: 4_200_000, TxPackets: 3_100_000},
				{Name: "br-wan", RxBytes: 9 << 30, TxBytes: 14 << 30, RxPackets: 1_800_000, TxPackets: 2_400_000},
			},
		}

	case OpTemplateExport:
		// 清单取自控制面给的模板信息：节点不认识"这个模板叫什么"以外的
		// 元数据，而控制面不认识"这个文件放在哪"。
		manifest := TemplateManifest{
			Name:       strParam(op.Params, "name"),
			Version:    versionOf(op.Params),
			DiskFormat: orString(strParam(op.Params, "disk_format"), "qcow2"),
			DiskSizeGB: intParam(op.Params, "disk_size"),
		}
		if s := strParam(op.Params, "family_name"); s != "" {
			manifest.FamilyName = s
		}
		version := manifest.Version
		if version <= 0 {
			version = 1
		}
		filename := manifest.Name + "-v" + strconv.Itoa(version) + ".tar.gz"
		data[TemplateExportDataKey] = TemplateExportInfo{
			RelPath:   "template-packages/" + filename,
			Filename:  filename,
			SizeBytes: int64(manifest.DiskSizeGB) * 1024 * 1024 * 1024 / 4,
			SHA256:    "mock-digest-" + filename,
			Manifest:  manifest,
		}

	case OpTemplateImportPreview, OpTemplateImport:
		// 清单由节点从包里读出来；mock 用包文件名推一个，形状必须完整
		// ——少一个字段的表现是界面上那一列空白。
		pkg := strParam(op.Params, "rel_path")
		base := pkg
		if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
			base = pkg[i+1:]
		}
		name := strings.TrimSuffix(strings.TrimSuffix(base, ".tar.gz"), ".tgz")
		if name == "" {
			name = "imported-template"
		}
		manifest := TemplateManifest{
			Name:       name,
			Version:    1,
			DiskFormat: "qcow2",
			DiskSizeGB: 20,
		}
		info := TemplateImportInfo{
			Manifest: manifest,
			// 摘要不符这条分支要**可达**：界面必须能渲染出"包内容与清单
			// 不符"这种失败，否则用户会在导入失败时看到一句空话。
			DigestMismatch: strings.Contains(pkg, "corrupt"),
			Message:        "模板包校验通过",
		}
		if op.Kind == OpTemplateImport {
			info.DiskPath = "/var/lib/k_cockpit/templates/" + name + ".qcow2"
			info.SizeGB = manifest.DiskSizeGB
			info.Format = manifest.DiskFormat
		}
		data[TemplateImportDataKey] = info
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
// 时间线形状与节点侧一致。
//
// ctx 取消时立即停止：任务被取消后还继续报阶段，会让界面在「已取消」之后
// 又冒出新的步骤。
func (m *MockClient) reportStages(ctx context.Context, op Operation) {
	if op.OnStage == nil {
		return
	}
	plan := stagePlan(op)
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

// guestMessage 给出一句面向用户的补充说明。
//
// 按动作分别写而不是给一句通用的「执行成功」：这四个动作的**结果形态**完全
// 不同（改了谁的密码、分区挂在哪儿、扩到多大），一句通用的话等于什么都没说。
func guestMessage(action string) string {
	switch action {
	case "password_online":
		return "已通过 Guest Agent 在线修改密码"
	case "password_offline":
		return "已离线挂载系统盘并修改密码"
	case "disk_attach":
		return "磁盘已分区、格式化并挂载到 /data"
	case "expand_disk":
		return "已加长虚拟磁盘并扩展文件系统"
	default:
		return "已完成"
	}
}

// mockNodeStats 按节点 ID 生成一组**稳定**的宿主机指标。
//
// 稳定（而不是随机）与 mockStats 同理：随机值会让轮询界面上的数字自己抖动，
// 看起来像真实负载在变化，而实际什么都没发生。
//
// 刻意让某些节点负载偏高、某些偏低，好让界面上「正常 / 偏高 / 吃紧」三种
// 呈现都能被看到。全都给 3% 会让告警样式永远没被渲染过——而那正是最需要
// 在演示前确认能正常显示的部分。
func mockNodeStats(nodeID int64) NodeStats {
	seed := uint32(nodeID*2654435761 + 12345)
	cores := int(seed%16) + 4
	cpu := float64(seed%95) + 3

	// 负载跟着 CPU 一起抬高，但**不与占用率成正比**：真实机器上两者确实
	// 常常不同步，而「占用率不高但负载很高」正是这条指标存在的意义。
	load := cpu / 100 * float64(cores) * 1.15

	return NodeStats{
		CPUPercent:     cpu,
		CPUCores:       cores,
		LoadAvg1:       load,
		LoadAvg5:       load * 0.92,
		LoadAvg15:      load * 0.85,
		MemTotalMB:     32768,
		MemUsedMB:      32768 * (int(seed%70) + 15) / 100,
		DiskTotalBytes: int64(seed%4000+1000) << 30,
		DiskUsedBytes:  int64(seed%4000+1000) << 30 * int64(seed%70+10) / 100,
		UptimeSeconds:  int64(seed%2592000) + 86400,
		AgentStartedAt: time.Now().Add(-time.Duration(seed%259200) * time.Second),
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
// strParam 从操作参数里取字符串；缺失时给空串。
//
// mock 不校验参数的合法性——那是控制面的职责。这里只需要一种"读出来"的
// 方式，而类型断言写得到处都是会让人误以为 mock 在意这些值。
func strParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

// intParam 读一个整数参数。JSON 反序列化后整数是 float64，
// 两种形状都要认——否则跨进程调用时它会永远是 0。
// versionOf 读版本号：没有或不是正数时按 1——版本号是"第几代"，
// 0 会让文件名变成 -v0，而那看起来像导出失败了。
func versionOf(params map[string]any) int {
	if v := intParam(params, "version"); v > 0 {
		return v
	}
	return 1
}

func intParam(params map[string]any, key string) int {
	switch v := params[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func orString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

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
// 注意：这意味着**离线分支在仅使用 mock 的阶段不会被触发**，
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
// 这属于 ADR-0007 接受的代价：控制台画面当前由 mock 提供。
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
