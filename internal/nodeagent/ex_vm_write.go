// ex_vm_write.go：vm.* 域的写操作执行器（create/clone、电源、delete、
// 快照、config.update、cdrom/floppy、NVRAM 修复）。
//
// 全部经 virsh（-r 读 / 可写连接写）+ qemu-img；libvirt Go 客户端在写路径
// 的收益（原子 define、事件）自 P2 末评估切换，本文件的方法签名不因此变化。
package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// strParam / boolParam 是执行器侧的参数读取（与控制面的同名助手语义一致）。
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
// **创建不自动开机**：与控制面的状态机一致（创建后的域是关机态，
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
			// 完整克隆要读整块模板盘；10s 的探测超时会让稍大的模板必然失败。
			_, err = sys.RunTimeout(ctx, qemuConvertTimeout, "qemu-img", "convert",
				"-O", extOf(spec.DiskFormat), spec.TemplateDiskPath, diskPath)
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
	// 数据盘此前只进域 XML 不落盘（review 抓出）——define 成功、开机
	// 必失败（libvirt 找不到盘文件）。在 define 之后补建：此时域已定义、
	// 盘路径已确定，建盘失败则整体报错（define 留着无盘域比删域更糟，
	// 但控制面任务失败后用户会重试——重试撞同名域，因此失败时回滚域）。
	_ = ensureQemuReadable(diskPath)
	created := []string{diskPath}
	for i, d := range spec.DataDisks {
		if d.SizeGB <= 0 {
			continue
		}
		dev := d.DevHint
		if dev == "" {
			dev = nextDataDev(i)
		}
		dp := dataDiskPath(spec.Name, dev, extOf(d.Format))
		if _, err := sys.Run(ctx, "qemu-img", "create", "-f", extOf(d.Format),
			dp, fmt.Sprintf("%dG", d.SizeGB)); err != nil {
			// 回滚要清掉**全部**已建盘：只删系统盘会让先建好的数据盘成
			// 孤儿文件（重试不会挡路——qemu-img create 对已存在文件静默
			// 重建——但垃圾留在宿主机盘上）。
			created = append(created, dp) // create 失败也可能留下半成品
			_, _ = sys.Run(ctx, "virsh", "undefine", "--nvram", "--managed-save", spec.Name)
			for _, p := range created {
				_ = os.Remove(p)
			}
			return failResult("创建数据盘 " + dev + " 失败: " + err.Error()), nil
		}
		created = append(created, dp)
		_ = ensureQemuReadable(dp)
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
		// 快照链的 backing 残留：快照/恢复之后域指向的是 overlay，原盘
		// 不在域 XML 里，下面的清单收集不到（真机实证：删除报成功、
		// <vm>.qcow2 留在宿主机上）。**先**沿 backing 链收集（原文件还在
		// 才读得到 backing——先删后探的话链永远为空），再统一删。只收
		// **以本机名开头**的文件：本产品的命名约定里那是这台机器的快照
		// overlay/原盘；linked clone 的模板盘不带本机前缀，绝不能顺链误删。
		residue := backingChainOf(ctx, sysDisk, op.Target+".")
		for _, path := range append([]string{sysDisk}, dataDiskPaths(dataDisks)...) {
			if path != "" {
				_ = os.Remove(path)
			}
		}
		for _, path := range residue {
			_ = os.Remove(path)
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
		if rerr := sys.Run0(ctx, "systemctl", "restart", "libvirtd"); rerr != nil {
			return failResult("重启 libvirtd 失败: " + rerr.Error()), nil
		}
		// 等服务真正起来再复核：重启窗口内 virsh 连不上会误判「域不在」
		//（DomainExists 用 dominfo，连接失败返回 false——复核假通过，
		// review 抓出的时序缺陷）。
		waitLibvirtdUp(ctx)
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

// backingFileOf 读盘文件的 backing file 路径（无 backing 或读取失败返回空）。
func backingFileOf(ctx context.Context, path string) string {
	out, err := sys.Run(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return ""
	}
	var info struct {
		BackingFile string `json:"backing-filename"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &info); jsonErr != nil {
		return ""
	}
	return info.BackingFile
}

// backingChainOf 沿 backing 链收集「可安全删除」的残留盘（不含 path 本身）。
// 只收文件名以 vmPrefix 开头的：那是本机创建时/快照时生成的盘；模板盘、
// 外部镜像不带本机前缀，链条在那里自然断掉。
func backingChainOf(ctx context.Context, path, vmPrefix string) []string {
	out := []string{}
	if path == "" {
		return out
	}
	cur := path
	for i := 0; i < 16; i++ { // 链深上限，防异常环
		backing := backingFileOf(ctx, cur)
		if backing == "" || !strings.HasPrefix(filepath.Base(backing), vmPrefix) {
			break
		}
		out = append(out, backing)
		cur = backing
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
	// 实际执行的种类要如实回传：控制面受理时的预测只是初值，UEFI(pflash)
	// 域不支持内部快照（libvirt 限制），关机 UEFI 域会被这里强制走
	// --disk-only 外部快照——记录不同步的话界面上 kind 就是错的（真机
	// 实测：关机 UEFI 建的快照记录成 internal，实际是外部 overlay）。
	kind := "internal"
	if state != "running" || !includeMemory {
		// 关机域或「不含内存」：--disk-only 走外部快照——UEFI(pflash)
		// 域不支持内部快照（libvirt 限制），--disk-only 是唯一可用路径。
		args = append(args, "--disk-only")
		kind = "external"
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
	// qemu 下次开机打不开——这里把链上盘文件的属主归一到 qemu 用户；
	// AppArmor 引导也在此兜底（升级部署后第一个操作可能是快照而非
	// 创建——review 抓出的引导缺席场景）。
	for _, d := range sys.ParseDomainDisks(xmlNow(ctx, op.Target)) {
		if d.SourceFile != "" {
			_ = ensureQemuReadable(d.SourceFile)
			ensureAppArmorAllowsImages(filepath.Dir(d.SourceFile))
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"domain_name":   name,
		"snapshot_kind": kind,
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
			ensureAppArmorAllowsImages(filepath.Dir(d.SourceFile))
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
		// setmem 不能超过域的最大内存——扩内存先抬天花板（setmaxmem）
		// 再设当前值（真机验证抓出：512MB 域调到 1024MB 被静默跳过）。
		err := setV("setmem", op.Target, fmtInt(v*1024), "--config")
		if err != nil {
			if e2 := setV("setmaxmem", op.Target, fmtInt(v*1024), "--config"); e2 == nil {
				err = setV("setmem", op.Target, fmtInt(v*1024), "--config")
			}
		}
		if err == nil {
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
		// Join 只做清洗不做约束：带 ../ 的 rel_path 会落到存储根之外
		//（guest 可挂载宿主机任意文件）——越界直接拒绝。
		if !strings.HasPrefix(isoPath, userdataRoot+"/") {
			return failResult("镜像路径越出用户存储根: " + isoPath), nil
		}
		// qemu 要打开的 ISO 在 images 之外——AppArmor 只放行了镜像目录
		//（review 抓出：挂 ISO 后开机 Permission denied，与快照链同根因）。
		ensureAppArmorAllowsImages(userdataRoot)
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
		// 与 attach 对称地带 --config：运行态 detach 对关机域直接报
		// "domain is not running"（review 抓出的不对称）。
		_, err = sys.Run(ctx, "virsh", "detach-device", "--config", op.Target, getStdinXML(op.Target, dev, bus))
	case "load":
		// 已有光驱 → update-device；未挂载 → attach-device。
		if cdromExists(ctx, op.Target, dev) {
			_, err = sys.Run(ctx, "virsh", "update-device", op.Target, getStdinXML(op.Target, dev, bus, isoPath))
		} else {
			_, err = sys.Run(ctx, "virsh", "attach-device", "--config", op.Target, getStdinXML(op.Target, dev, bus, isoPath))
		}
	case "attach":
		_, err = sys.Run(ctx, "virsh", "attach-device", "--config", op.Target, getStdinXML(op.Target, dev, bus, isoPath))
	case "eject":
		if cdromExists(ctx, op.Target, dev) {
			_, err = sys.Run(ctx, "virsh", "update-device", op.Target, getStdinXML(op.Target, dev, bus, ""))
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
// getStdinXML 把设备片段经临时文件交给 virsh attach/update-device。
// 文件名带域名：不同 VM 的同名光驱（sda/hdb 很常见）并发操作时共用
// 一个 /tmp 文件会互相覆盖（review 抓出的碰撞）。
func getStdinXML(vm, dev, bus string, isoPath ...string) string {
	frag := cdromFragment(dev, bus, "")
	if len(isoPath) > 0 {
		frag = cdromFragment(dev, bus, isoPath[0])
	}
	f := filepath.Join(os.TempDir(), "kc-cdrom-"+vm+"-"+dev+".xml")
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
// 做法是**把 NVRAM 变量文件重置为固件模板**：保留当前文件为 .bak，
// 用域定义里 `<nvram template='...'>` 指向的模板覆盖它。下次开机固件
// 会重新枚举启动项；这是比直接删文件更安全的路径（目标路径始终存在）。
// 必须关机操作：运行中的域仍持有旧 NVRAM 文件。
func vmNVRAMRepair(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "nvram", Name: "修复 UEFI 启动项", Index: 1, Total: 1})
	if running, err := vmRunning(ctx, op.Target); err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	} else if running {
		return failResult("NVRAM 修复需要先关机：运行中的域仍持有旧变量文件"), nil
	}
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	target, template, ok := nvramPathAndTemplate(xmlText)
	if !ok {
		return failResult("该虚拟机没有 NVRAM（不是 UEFI 域），无需修复"), nil
	}
	if template == "" {
		return failResult("域定义没有记录固件模板路径，无法安全重建 NVRAM"), nil
	}
	if st, err := os.Stat(template); err != nil || st.IsDir() {
		return failResult("固件模板不可用: " + template), nil
	}
	if st, err := os.Stat(target); err == nil && !st.IsDir() {
		backup := target + ".bak-" + strconv.FormatInt(time.Now().Unix(), 10)
		if err := copyFile(ctx, target, backup); err != nil {
			return failResult("备份 NVRAM 失败: " + err.Error()), nil
		}
	}
	tmp := target + ".kc-repair-tmp"
	_ = os.Remove(tmp)
	if err := copyFile(ctx, template, tmp); err != nil {
		return failResult("重置 NVRAM 失败: " + err.Error()), nil
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return failResult("替换 NVRAM 失败: " + err.Error()), nil
	}
	if err := ensureQemuReadable(target); err != nil {
		return failResult("设置 NVRAM 属主失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.NVRAMDataKey: agent.NVRAMRepairInfo{
			Message:      "NVRAM 已重置为固件模板；下次启动会重新枚举启动项",
			RebootNeeded: true,
		},
	}}, nil
}

// nvramPathAndTemplate 从域定义取 NVRAM 目标路径与固件模板路径。
func nvramPathAndTemplate(xmlText string) (target, template string, ok bool) {
	i := strings.Index(xmlText, "<nvram")
	if i < 0 {
		return "", "", false
	}
	end := strings.Index(xmlText[i:], "</nvram>")
	if end < 0 {
		return "", "", false
	}
	block := xmlText[i : i+end+len("</nvram>")]
	template = attrValue(block, "template")
	if j := strings.Index(block, ">"); j >= 0 {
		body := block[j+1:]
		if k := strings.Index(body, "</nvram>"); k >= 0 {
			target = strings.TrimSpace(body[:k])
		}
	}
	return target, template, target != ""
}

// vmFloppyApply 实现 vm.floppy.apply（挂载/弹出软盘）。
//
// 软盘不支持热插拔：libvirt 对 fdc 设备的 attach 只在关机态可靠。
// 创建流程在开机前调用，因此正常路径满足；运行中调用如实拒绝。
func vmFloppyApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "floppy", Name: "调整软驱", Index: 1, Total: 1})
	action := strParam(op.Params, "action")
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}

	switch action {
	case "attach":
		rel := strParam(op.Params, "rel_path")
		if rel == "" {
			return failResult("缺少 rel_path：控制面必须把存储文件解析为节点相对路径"), nil
		}
		if filepath.IsAbs(rel) {
			return failResult("rel_path 必须是相对于用户存储根的路径"), nil
		}
		source := filepath.Join(userdataRoot, rel)
		if source != userdataRoot && !strings.HasPrefix(source, userdataRoot+"/") {
			return failResult("软盘路径越出用户存储根: " + source), nil
		}
		if st, err := os.Stat(source); err != nil || st.IsDir() {
			return failResult("软盘镜像不存在: " + source), nil
		}
		if running, err := vmRunning(ctx, op.Target); err != nil {
			return failResult("无法确认虚拟机状态: " + err.Error()), nil
		} else if running {
			return failResult("软盘不支持热插拔：请先关机再挂载"), nil
		}
		if block, ok := floppyBlockOf(xmlText); ok {
			// 已经有一块软盘：同一路径视为幂等成功，不同路径要求先弹出。
			if strings.Contains(block, xmlEsc(source)) {
				return &agent.Result{Success: true, Message: "软盘已挂载"}, nil
			}
			return failResult("虚拟机已有一块软盘，请先弹出"), nil
		}
		if err := ensureQemuReadable(source); err != nil {
			return failResult("设置软盘镜像属主失败: " + err.Error()), nil
		}
		ensureAppArmorAllowsImages(userdataRoot)
		if !hasFDCController(xmlText) {
			xmlText = addFDCController(xmlText)
			if err := defineXML(ctx, xmlText); err != nil {
				return failResult("添加软驱控制器失败: " + err.Error()), nil
			}
		}
		frag := floppyFragment(source)
		path, cleanup, err := writeTempXML("floppy-"+op.Target, frag)
		if err != nil {
			return failResult("写入软盘定义失败: " + err.Error()), nil
		}
		defer cleanup()
		if _, err := virshWrite(ctx, "attach-device", "--config", op.Target, path); err != nil {
			return failResult("挂载软盘失败: " + err.Error()), nil
		}
		if !floppyExists(ctx, op.Target) {
			return failResult("libvirt 接受了挂载命令，但域定义里没有软盘设备"), nil
		}
		return &agent.Result{Success: true, Message: "软盘已挂载"}, nil

	case "eject", "remove":
		block, ok := floppyBlockOf(xmlText)
		if !ok {
			return &agent.Result{Success: true, Message: "软盘已不存在（幂等成功）"}, nil
		}
		path, cleanup, err := writeTempXML("floppy-old-"+op.Target, block)
		if err != nil {
			return failResult("写入软盘定义失败: " + err.Error()), nil
		}
		defer cleanup()
		if _, err := virshWrite(ctx, "detach-device", "--config", op.Target, path); err != nil {
			return failResult("弹出软盘失败: " + err.Error()), nil
		}
		return &agent.Result{Success: true, Message: "软盘已弹出"}, nil

	default:
		return failResult("未知软盘动作: " + action), nil
	}
}

// floppyFragment 渲染一块软盘设备。
func floppyFragment(source string) string {
	return "<disk type='file' device='floppy'>\n" +
		"      <driver name='qemu' type='raw'/>\n" +
		"      <source file='" + xmlEsc(source) + "'/>\n" +
		"      <target dev='fda' bus='fdc'/>\n" +
		"    </disk>"
}

// floppyBlockOf 返回域定义里的软盘设备块。
func floppyBlockOf(xmlText string) (string, bool) {
	for _, block := range xmlBlocks(xmlText, "<disk ", "</disk>") {
		disks := sys.ParseDomainDisks(block)
		if len(disks) > 0 && disks[0].IsFloppy {
			return block, true
		}
	}
	return "", false
}

// floppyExists 报告持久定义里是否有软盘。
func floppyExists(ctx context.Context, vm string) bool {
	xmlText, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return false
	}
	_, ok := floppyBlockOf(xmlText)
	return ok
}

// waitLibvirtdUp 轮询等待 libvirtd 可服务（最多 ~10s）。
func waitLibvirtdUp(ctx context.Context) {
	for i := 0; i < 10; i++ {
		if sys.RunOK(ctx, "virsh", "-r", "-c", "qemu:///system", "hostname") {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
