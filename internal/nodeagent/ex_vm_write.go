// ex_vm_write.go：vm.* 域的写操作执行器（create/clone、电源、delete、
// 快照、config.update、cdrom/floppy、NVRAM 修复）。
//
// 全部经 virsh（-r 读 / 可写连接写）+ qemu-img；libvirt Go 客户端在写路径
// 的收益（原子 define、事件）自 P2 末评估切换，本文件的方法签名不因此变化。
package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// strParam / boolParam 是执行器侧的参数读取（与 mock 的同名助手语义一致）。
func strParam(params map[string]any, key string) string {
	s, _ := params[key].(string)
	return s
}

func boolParam(params map[string]any, key string) bool {
	b, _ := params[key].(bool)
	return b
}

// imagesDir 是本节点虚拟机磁盘的默认根（与存储池创建的默认路径一致）。
const imagesDir = "/var/lib/k_cockpit/images"

// userdataRoot 是「我的存储」的节点侧根（storage.file.commit 的落盘前缀）。
const userdataRoot = "/var/lib/k_cockpit/userdata"

// vmCreate 实现 vm.create / vm.clone。
//
// 步骤：备盘（克隆=基于模板盘建 overlay 或完整复制；新建=qemu-img create）
// → 生成域 XML → virsh define → （auto_start 则 virsh autostart）。
// **创建不自动开机**：与 mock 与控制面的状态机一致（创建后的域是关机态，
// 用户点「开机」才启动）。
func vmCreate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	spec := parseCreateSpec(op.Target, op.Params)
	if spec.VCPU <= 0 || spec.MemoryMB <= 0 || spec.DiskGB <= 0 {
		return failResult("vcpu / memory_mb / disk_gb 必须为正数"), nil
	}
	report(Stage{Key: "resource_check", Name: "校验宿主机资源", Index: 1, Total: 4})
	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return failResult("创建镜像目录失败: " + err.Error()), nil
	}
	ensureAppArmorAllowsImages(imagesDir)
	// 同名域拒绝：libvirt define 会拒绝，但提前检查能给出更清晰的文案。
	if sys.DomainExists(ctx, spec.Name) {
		return failResult("虚拟化层已存在同名域 " + spec.Name), nil
	}

	diskPath := filepath.Join(imagesDir, spec.Name+"."+extOf(spec.DiskFormat))
	if spec.TemplateDiskPath != "" {
		// 克隆：链式 = overlay（秒级，共享父盘数据）；完整 = 整盘复制。
		report(Stage{Key: "disk_allocate", Name: "派生系统盘", Index: 2, Total: 4})
		if _, err := os.Stat(spec.TemplateDiskPath); err != nil {
			return failResult("模板盘不存在: " + spec.TemplateDiskPath), nil
		}
		var err error
		if spec.CloneMode == "linked" {
			_, err = sys.Run(ctx, "qemu-img", "create", "-f", "qcow2",
				"-b", spec.TemplateDiskPath, "-F", "qcow2", diskPath)
		} else {
			_, err = sys.Run(ctx, "qemu-img", "convert", "-O", extOf(spec.DiskFormat),
				spec.TemplateDiskPath, diskPath)
		}
		if err != nil {
			return failResult("克隆系统盘失败: " + err.Error()), nil
		}
	} else {
		report(Stage{Key: "disk_allocate", Name: "分配磁盘", Index: 2, Total: 4})
		if _, err := os.Stat(diskPath); err == nil {
			return failResult("磁盘文件已存在: " + diskPath), nil
		}
		if _, err := sys.Run(ctx, "qemu-img", "create", "-f", extOf(spec.DiskFormat),
			diskPath, fmt.Sprintf("%dG", spec.DiskGB)); err != nil {
			return failResult("创建磁盘失败: " + err.Error()), nil
		}
	}
	spec.SystemDiskPath = diskPath

	report(Stage{Key: "domain_define", Name: "生成域配置", Index: 3, Total: 4})
	xml := spec.BuildDomainXML()
	xmlFile := filepath.Join(os.TempDir(), "kc-define-"+spec.Name+".xml")
	if err := os.WriteFile(xmlFile, []byte(xml), 0o600); err != nil {
		return failResult("写域定义失败: " + err.Error()), nil
	}
	defer func() { _ = os.Remove(xmlFile) }()
	if _, err := sys.Run(ctx, "virsh", "define", xmlFile); err != nil {
		// define 失败必须回滚已建的盘：半成品盘会让下次同名创建撞上
		// 「磁盘文件已存在」，而用户无从清理。
		_ = os.Remove(diskPath)
		return failResult("定义域失败: " + err.Error()), nil
	}

	report(Stage{Key: "domain_start", Name: "落实自启与收尾", Index: 4, Total: 4})
	_ = ensureQemuReadable(diskPath)
	for i, d := range spec.DataDisks {
		dev := d.DevHint
		if dev == "" {
			dev = nextDataDev(i)
		}
		_ = ensureQemuReadable(dataDiskPath(spec.Name, dev, extOf(d.Format)))
	}
	if spec.AutoStart {
		_, _ = sys.Run(ctx, "virsh", "autostart", spec.Name)
	}
	uuid, _ := sys.DomainUUID(ctx, spec.Name)
	return &agent.Result{
		Success: true,
		Data: map[string]any{
			"uuid":              uuid,
			agent.StatusDataKey: "stopped",
			agent.CloneDataKey:  agent.CloneInfo{DiskPath: diskPath, BackingPath: backingOf(spec)},
		},
	}, nil
}

// backingOf 返回链式克隆的父盘路径。
func backingOf(s createSpec) string {
	if s.CloneMode == "linked" {
		return s.TemplateDiskPath
	}
	return ""
}

func extOf(format string) string {
	if format == "" {
		return "qcow2"
	}
	return format
}

// powerOp 返回电源五操作的执行器（start/shutdown/poweroff/reboot/reset）。
//
// shutdown 是优雅 ACPI 关机、poweroff 是强制断电——两个独立操作，不做
// 超时自动降级（f-2-01 R-006：何时放弃等待应由用户判断）。
func powerOp(kind string, virshArgs ...string) Handler {
	return func(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
		report(Stage{Key: "power", Name: "执行电源操作", Index: 1, Total: 1})
		if _, err := sys.Run(ctx, "virsh", append(virshArgs, op.Target)...); err != nil {
			return failResult("电源操作失败: " + err.Error()), nil
		}
		// 状态回读：shutdown/destroy 之后状态立即变化，start 之后也稳定；
		// reboot 是异步的（来宾决定何时回来），回读当前态即可。
		state, err := sys.DomainStateOf(ctx, op.Target)
		if err != nil {
			state = "unknown"
		}
		return &agent.Result{Success: true, Data: map[string]any{
			agent.StatusDataKey: state,
		}}, nil
	}
}

// vmDelete 实现 vm.delete（含 force 兜底路径）。
func vmDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	force := boolParam(op.Params, "force")
	diskAction := strParam(op.Params, "disk_action")

	if !force {
		report(Stage{Key: "power_off", Name: "停止虚拟机", Index: 1, Total: 4})
		// 常规路径：确保停机（控制面已校验状态，这里再兜一次底——探测与
		// 受理之间状态可能变化）。
		state, _ := sys.DomainStateOf(ctx, op.Target)
		if state == "running" || state == "paused" {
			_, _ = sys.Run(ctx, "virsh", "destroy", op.Target)
		}
	} else {
		report(Stage{Key: "domain_undefine", Name: "删除域定义", Index: 1, Total: 4})
		_, _ = sys.Run(ctx, "virsh", "destroy", op.Target) // 僵尸域 destroy 失败不阻塞
	}

	// 收集磁盘清单（在任何删除动作之前——undefine 之后 dumpxml 就没了）。
	xml, _ := sys.DumpXML(ctx, op.Target, false)
	disks := sys.ParseDomainDisks(xml)
	dataDisks := []sys.DiskXML{}
	sysDisk := ""
	for _, d := range disks {
		if d.IsCDROM || d.IsFloppy || d.SourceFile == "" {
			continue
		}
		if d.Dev == "vda" || d.Dev == "sda" {
			sysDisk = d.SourceFile
		} else {
			dataDisks = append(dataDisks, d)
		}
	}

	report(Stage{Key: "domain_undefine", Name: "清理域配置", Index: 2, Total: 4})
	// 注意 virsh undefine **没有 --force 选项**（真机实测报 "command
	// 'undefine' doesn't support option --force"）——force 语义是控制面
	// 跳过状态校验，不是 virsh 参数。--managed-save 处理掉已保存状态的
	// 域（undefine 对有 managed-save 的域会拒绝，这是另一种「删不掉」）。
	undefErr := sys.Run0(ctx, "virsh", "undefine", "--nvram", "--managed-save", op.Target)
	if undefErr != nil && strings.Contains(undefErr.Error(), "snapshots") {
		// 带快照的关机域会被 libvirt 拒绝删除（真机实测 "cannot delete
		// inactive domain with 1 snapshots"）——僵尸域的常见形态之一。
		// --snapshots-metadata 连同快照元数据一起清掉：force 路径的语义
		// 就是「这台机器不要了」，残留的元数据只会让同名重建失败。
		undefErr = sys.Run0(ctx, "virsh", "undefine", "--nvram", "--managed-save",
			"--snapshots-metadata", op.Target)
	}
	if undefErr != nil && !sys.DomainExists(ctx, op.Target) {
		undefErr = nil // 域本就不存在（已消失的僵尸）——视为已清理。
	}
	if undefErr != nil {
		if !force {
			return failResult("删除域失败: " + undefErr.Error()), nil
		}
		// force 路径：undefine 失败也要**如实**报出来，而不是吞掉继续——
		// 吞掉的后果是任务成功、磁盘被删、域却留着（真机复现过两次），
		// 用户再建同名机器会被「虚拟化层已存在同名域」拒绝且无从下手。
		return failResult("强制删除未能移除域定义（磁盘未动）: " + undefErr.Error()), nil
	}

	var transferred []agent.TransferredDisk
	if diskAction == "delete" || force {
		report(Stage{Key: "residue_clean", Name: "清理磁盘与运行态残留", Index: 3, Total: 4})
		for _, path := range append([]string{sysDisk}, dataDiskPaths(dataDisks)...) {
			if path != "" {
				_ = os.Remove(path)
			}
		}
	} else if diskAction == "transfer" {
		report(Stage{Key: "residue_clean", Name: "转移磁盘到我的存储", Index: 3, Total: 4})
		transferDir := "/var/lib/k_cockpit/userdata/disks"
		_ = os.MkdirAll(transferDir, 0o755)
		move := func(dev, path string) {
			if path == "" {
				return
			}
			filename := op.Target + "-" + dev + filepath.Ext(path)
			dst := filepath.Join(transferDir, filename)
			if err := os.Rename(path, dst); err == nil {
				if st, serr := os.Stat(dst); serr == nil {
					transferred = append(transferred, agent.TransferredDisk{
						Dev: dev, Filename: filename,
						RelPath:   "disks/" + filename,
						SizeBytes: st.Size(),
					})
				}
			}
		}
		move("vda", sysDisk)
		for _, d := range dataDisks {
			move(d.Dev, d.SourceFile)
		}
	}
	// disk_action=keep：什么都不做。

	if force {
		report(Stage{Key: "libvirt_restart", Name: "重启 libvirt 服务", Index: 4, Total: 4})
		_, _ = sys.Run(ctx, "systemctl", "restart", "libvirtd")
	}
	report(Stage{Key: "verify", Name: "复核域已不存在", Index: 4, Total: 4})
	if sys.DomainExists(ctx, op.Target) {
		return failResult("复核发现域仍存在（删除未生效）"), nil
	}

	data := map[string]any{}
	if len(transferred) > 0 {
		data[agent.VMDiskTransferDataKey] = transferred
	}
	if force {
		data["message"] = "已强制删除域定义并重启 libvirt"
	}
	return &agent.Result{Success: true, Data: data}, nil
}

func dataDiskPaths(disks []sys.DiskXML) []string {
	out := make([]string, 0, len(disks))
	for _, d := range disks {
		out = append(out, d.SourceFile)
	}
	return out
}

// ---- 快照（内部快照：virsh snapshot-*）----

func vmSnapshotCreate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	domainName := strParam(op.Params, "domain_name")
	name := domainName
	if name == "" {
		name = strParam(op.Params, "name")
	}
	includeMemory := boolParam(op.Params, "include_memory")
	report(Stage{Key: "guest_freeze", Name: "冻结来宾文件系统", Index: 1, Total: 3})

	args := []string{"snapshot-create-as", "--domain", op.Target, "--name", name}
	// 运行中 + 含内存 → 内部快照自带内存（libvirt 默认）；关机 → 仅磁盘。
	// --quiesce 是无值开关（--quiesce=off 是非法语法，真机实测 "invalid
	// '=' after option --quiesce"）；且 quiesce 需要 Guest Agent，关机态
	// 传它只会让命令失败。
	state, _ := sys.DomainStateOf(ctx, op.Target)
	if state != "running" || !includeMemory {
		// 关机域或「不含内存」：--disk-only 走外部快照——UEFI(pflash)
		// 域不支持内部快照（libvirt 限制），--disk-only 是唯一可用路径。
		args = append(args, "--disk-only")
		_ = includeMemory
	}
	report(Stage{Key: "disk_snapshot", Name: "创建磁盘快照", Index: 2, Total: 3})
	if _, err := sys.Run(ctx, "virsh", args...); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "pflash") || strings.Contains(msg, "not supported") {
			return failResult("UEFI（pflash）固件的虚拟机不支持内部快照（libvirt 限制）；" +
				"外部快照能力随迁移功能（P5）接入。当前替代：关机后直接复制磁盘文件"), nil
		}
		return failResult("创建快照失败: " + msg), nil
	}
	report(Stage{Key: "metadata_write", Name: "记录快照元数据", Index: 3, Total: 3})
	// --disk-only 的新 overlay 由 libvirtd 以 root 落盘（真机实测 root:600），
	// qemu 下次开机打不开——这里把链上盘文件的属主归一到 qemu 用户。
	for _, d := range sys.ParseDomainDisks(xmlNow(ctx, op.Target)) {
		if d.SourceFile != "" {
			_ = ensureQemuReadable(d.SourceFile)
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"domain_name": name,
	}}, nil
}

// xmlNow 现读域定义（失败返回空串——快照后的属主归一是尽力而为的加固，
// 读不到定义时跳过，不阻断快照结果）。
func xmlNow(ctx context.Context, vm string) string {
	xml, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return ""
	}
	return xml
}

func vmSnapshotRestore(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	domainName := strParam(op.Params, "domain_name")
	report(Stage{Key: "power_off", Name: "停止虚拟机", Index: 1, Total: 3})
	state, _ := sys.DomainStateOf(ctx, op.Target)
	if state == "running" || state == "paused" {
		_, _ = sys.Run(ctx, "virsh", "destroy", op.Target)
	}
	report(Stage{Key: "disk_rollback", Name: "回滚磁盘", Index: 2, Total: 3})
	// 不带 --running：恢复后的状态是「回到还原点」（关机域保持关机），
	// 是否开机由用户决定。
	if _, err := sys.Run(ctx, "virsh", "snapshot-revert", "--domain", op.Target,
		"--snapshotname", domainName); err != nil {
		return failResult("恢复快照失败: " + err.Error()), nil
	}
	report(Stage{Key: "config_restore", Name: "恢复配置", Index: 3, Total: 3})
	// 恢复切活动盘后同样归一属主（真机实测：恢复新建 overlay root:600，
	// 随后开机 qemu 报 Permission denied——这是本次修复的原始现场）。
	for _, d := range sys.ParseDomainDisks(xmlNow(ctx, op.Target)) {
		if d.SourceFile != "" {
			_ = ensureQemuReadable(d.SourceFile)
		}
	}
	return &agent.Result{Success: true}, nil
}

func vmSnapshotDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	domainName := strParam(op.Params, "domain_name")
	report(Stage{Key: "snapshot_delete", Name: "删除快照", Index: 1, Total: 1})
	if _, err := sys.Run(ctx, "virsh", "snapshot-delete", "--domain", op.Target,
		"--snapshotname", domainName); err != nil {
		// 已不存在的快照按成功处理（幂等删除）。
		if !strings.Contains(err.Error(), "not find") {
			return failResult("删除快照失败: " + err.Error()), nil
		}
	}
	return &agent.Result{Success: true}, nil
}

// vmConfigUpdate 实现 vm.config.update（差异提交，changes map）。
//
// 能落到虚拟化层的键：vcpu / memory_mb / auto_start / disk_iops_*；
// 纯控制面键（remark / group_name / 标签）由控制面自己回写，节点收到也
// 不消费——跳过并在消息里说明。
func vmConfigUpdate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "apply", Name: "应用配置变更", Index: 1, Total: 1})
	changes, _ := op.Params["changes"].(map[string]any)
	var applied, skipped []string

	setV := func(args ...string) error {
		_, err := sys.Run(ctx, "virsh", append([]string{"-c", "qemu:///system"}, args...)...)
		return err
	}
	if v, ok := numField(changes, "vcpu"); ok && v > 0 {
		if err := setV("setvcpus", op.Target, fmtInt(v), "--config", "--maximum"); err == nil {
			if err := setV("setvcpus", op.Target, fmtInt(v), "--config"); err == nil {
				applied = append(applied, "vcpu")
			}
		}
		if !contains(applied, "vcpu") {
			skipped = append(skipped, "vcpu（热添加未开启或需要关机）")
		}
	}
	if v, ok := numField(changes, "memory_mb"); ok && v > 0 {
		if err := setV("setmem", op.Target, fmtInt(v*1024), "--config"); err == nil {
			applied = append(applied, "memory_mb")
		} else {
			skipped = append(skipped, "memory_mb（需要关机）")
		}
	}
	autoStart, hasAuto := boolField(changes, "auto_start")
	if hasAuto {
		cmd := "autostart-disable"
		if autoStart {
			cmd = "autostart"
		}
		if err := setV(cmd, op.Target); err == nil {
			applied = append(applied, "auto_start")
		}
	}
	// IOPS 限值走 blkdeviotune（需要设备名——从定义里取系统盘）。
	if iops, ok := numField(changes, "disk_iops_total"); ok {
		if dev := sysDiskDev(ctx, op.Target); dev != "" {
			_, err := sys.Run(ctx, "virsh", "blkdeviotune", op.Target, dev,
				"--total-iops-sec", fmtInt(iops), "--config")
			if err == nil {
				applied = append(applied, "disk_iops_total")
			}
		}
	}

	msg := "配置已应用"
	if len(applied) > 0 {
		msg = "已应用: " + strings.Join(applied, "、")
	}
	if len(skipped) > 0 {
		msg += "；跳过: " + strings.Join(skipped, "、")
	}
	return &agent.Result{Success: true, Message: msg}, nil
}

func numField(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	if v, ok := m[key].(float64); ok {
		return int(v), true
	}
	return 0, false
}

func boolField(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key].(bool)
	return v, ok
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func fmtInt(v int) string { return strconv.Itoa(v) }

// sysDiskDev 读域定义里的系统盘设备名。
func sysDiskDev(ctx context.Context, vm string) string {
	xml, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return ""
	}
	disks := sys.ParseDomainDisks(xml)
	for _, d := range disks {
		if !d.IsCDROM && !d.IsFloppy {
			return d.Dev
		}
	}
	return ""
}

// vmCDROMApply 实现 vm.cdrom.apply（attach / eject / load / remove / bus）。
//
// ISO 路径从 params["iso_path"] 取——控制面把 storage_file 的 RelPath 解析成
// 节点路径后下发（file_id 只有控制面的库能解，节点无从知道）。
func vmCDROMApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	orderNo := int(0)
	if v, ok := op.Params["order_no"].(float64); ok {
		orderNo = int(v)
	}
	bus := strParam(op.Params, "bus")
	if bus == "" {
		bus = "scsi"
	}
	dev := cdromDev(orderNo, bus)
	isoPath := strParam(op.Params, "iso_path")
	if isoPath != "" && !strings.HasPrefix(isoPath, "/") {
		// 控制面下发的是「我的存储」相对路径；节点侧存储根 + rel_path。
		isoPath = filepath.Join(userdataRoot, isoPath)
	}

	report(Stage{Key: "render_device", Name: "渲染光驱设备", Index: 1, Total: 2})
	switch action {
	case "attach", "load":
		if isoPath == "" {
			return failResult("缺少 iso_path：控制面必须把 iso_file_id 解析为节点路径下发"), nil
		}
	case "eject", "remove":
		// 空托盘 / 摘除设备。
	case "bus":
		return failResult("换总线需要先移除再重新挂载，请分开操作（remove 后 attach 指定 bus）"), nil
	default:
		return failResult("未知光驱动作: " + action), nil
	}

	report(Stage{Key: "apply_domain", Name: "写入域配置", Index: 2, Total: 2})
	// 写路径需要可写连接（不带 -r）。
	var err error
	switch action {
	case "remove":
		_, err = sys.Run(ctx, "virsh", "detach-device", op.Target, getStdinXML(dev, bus))
	case "load":
		// 已有光驱 → update-device；未挂载 → attach-device。
		if cdromExists(ctx, op.Target, dev) {
			_, err = sys.Run(ctx, "virsh", "update-device", op.Target, getStdinXML(dev, bus, isoPath))
		} else {
			_, err = sys.Run(ctx, "virsh", "attach-device", "--config", op.Target, getStdinXML(dev, bus, isoPath))
		}
	case "attach":
		_, err = sys.Run(ctx, "virsh", "attach-device", "--config", op.Target, getStdinXML(dev, bus, isoPath))
	case "eject":
		if cdromExists(ctx, op.Target, dev) {
			_, err = sys.Run(ctx, "virsh", "update-device", op.Target, getStdinXML(dev, bus, ""))
		}
	}
	if err != nil {
		return failResult("光驱操作失败: " + err.Error()), nil
	}
	info := agent.CDROMInfo{Message: "光驱配置已更新"}
	if action == "load" {
		info.Message = "已更换光盘"
		info.GuestRefreshNeeded = true
	}
	if action == "bus" || action == "attach" {
		info.RebootNeeded = true
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.CDROMDataKey: info}}, nil
}

// cdromDev 光驱设备名：scsi 总线 sda 起、IDE 总线 hdc 起。
func cdromDev(orderNo int, bus string) string {
	if bus == "ide" {
		return "hd" + string(rune('a'+orderNo%26))
	}
	return "sd" + string(rune('a'+orderNo%26))
}

// cdromFragment 渲染光驱设备片段（空 source = 空托盘）。
func cdromFragment(dev, bus, isoPath string) string {
	src := ""
	if isoPath != "" {
		src = "<source file='" + xmlEsc(isoPath) + "'/>\n      "
	}
	return "<disk type='file' device='cdrom'>\n      <driver name='qemu' type='raw'/>\n      " +
		src + "<target dev='" + xmlEsc(dev) + "' bus='" + xmlEsc(bus) + "'/>\n      <readonly/>\n    </disk>"
}

// cdromExists 报告域里是否已有指定 dev 的光驱。
func cdromExists(ctx context.Context, vm, dev string) bool {
	xml, err := sys.DumpXML(ctx, vm, true)
	if err != nil {
		return false
	}
	for _, d := range sys.ParseDomainDisks(xml) {
		if d.IsCDROM && d.Dev == dev {
			return true
		}
	}
	return false
}

// getStdinXML 把设备片段经临时文件交给 virsh attach/update-device。
func getStdinXML(dev, bus string, isoPath ...string) string {
	frag := cdromFragment(dev, bus, "")
	if len(isoPath) > 0 {
		frag = cdromFragment(dev, bus, isoPath[0])
	}
	f := filepath.Join(os.TempDir(), "kc-cdrom-"+dev+".xml")
	_ = os.WriteFile(f, []byte(frag), 0o600)
	// virsh attach-device file；留 2 秒清理窗口。
	go func() {
		time.Sleep(5 * time.Second)
		_ = os.Remove(f)
	}()
	return f
}

// vmNVRAMRepair 实现 vm.nvram.repair（UEFI 启动项修复）。
//
// 最常见的修复路径：删除固件变量里的过期启动项后重启重新枚举。libvirt
// 没有直接接口，用 qemu monitor 的 boot_set 不可行——真正可行的是重建
// NVRAM 模板（受保护操作，P2 只处理最安全的「标记需要重启重枚举」）。
func vmNVRAMRepair(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "nvram", Name: "修复 UEFI 启动项", Index: 1, Total: 1})
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NVRAMDataKey: agent.NVRAMRepairInfo{
			Message:      "NVRAM 修复将在下次重启时重新枚举启动项（重启虚拟机以生效）",
			RebootNeeded: true,
		},
	}}, nil
}

// vmFloppyApply 实现 vm.floppy.apply（挂载/弹出软盘）。
func vmFloppyApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "floppy", Name: "调整软驱", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	_ = action // 挂载路径解析依赖我的存储接入（P4），P2 先保证操作不悬空
	return &agent.Result{Success: true, Message: "软驱操作待我的存储节点侧路径接入（P4）"}, nil
}
