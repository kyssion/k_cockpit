// ex_vm_edit.go：虚拟机编辑类执行器（vm.adopt / vm.xml.apply /
// vm.disk.* / vm.disks.independent / vm.interface.change）。
//
// 这些操作在 P2 时只有 mock 实现：控制面已经完整建模，节点侧却从未接真。
// 本文件把它们接到 libvirt，原则与其它写执行器一致：
//   - 状态以节点为准：运行中/关机先探测，不凭控制面投影猜；
//   - 幂等：重复 detach 已不存在的设备视为成功，重复 attach 先查重；
//   - 失败带节点侧原始原因（libvirt 的报错里有元素名与行号）。
package nodeagent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

const (
	// qemuConvertTimeout 给整盘转换/合并：几十 GB 的镜像在慢盘上可能跑
	// 几十分钟，用探测命令的 10s 上限会被必然超时。
	qemuConvertTimeout = 6 * time.Hour
	// blockCopyTimeout 给热迁移 blockcopy：同样按整盘时长放宽。
	blockCopyTimeout = 6 * time.Hour
)

// paramInt 读取 JSON 反序列化后的整数值（float64）或直接传入的 int。
func paramInt(params map[string]any, key string) (int, bool) {
	switch v := params[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	default:
		return 0, false
	}
}

// vmRunning 探测域是否在运行。探测失败返回错误——不猜。
func vmRunning(ctx context.Context, vm string) (bool, error) {
	state, err := sys.DomainStateOf(ctx, vm)
	if err != nil {
		return false, err
	}
	return state == "running", nil
}

// writeTempXML 把设备片段或域定义落到临时文件，供 virsh 以文件参数读取。
//
// 文件名带域名与纳秒时间戳：并发操作同一台虚拟机的不同设备时不能互相覆盖。
func writeTempXML(prefix, content string) (string, func(), error) {
	f, err := os.CreateTemp("", "kc-"+sanitizeFileName(prefix)+"-*.xml")
	if err != nil {
		return "", func() {}, err
	}
	path := f.Name()
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", func() {}, err
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// sanitizeFileName 只保留文件名安全字符（域名里可能有 . 与 -）。
func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "edit"
	}
	return out
}

// virshWrite 执行一条可写连接（去掉 -r）的 virsh 命令。
func virshWrite(ctx context.Context, args ...string) (string, error) {
	return sys.Run(ctx, "virsh", append([]string{"-c", "qemu:///system"}, args...)...)
}

// defineXML 把一份完整域定义写入持久配置。
func defineXML(ctx context.Context, xmlText string) error {
	path, cleanup, err := writeTempXML("define", xmlText)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = virshWrite(ctx, "define", path)
	return err
}

// vmAdopt 实现 vm.adopt：把存量域登记到托管集合并回传 UUID。
//
// 节点侧没有额外的“托管集合”状态：控制面的记录就是托管集合，节点只负责
// 提供唯一且稳定的对账键。真正的状态上报与指标采集按域扫描，不依赖标记。
func vmAdopt(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "resolve_uuid", Name: "读取域标识", Index: 1, Total: 1})
	uuid, err := sys.DomainUUID(ctx, op.Target)
	if err != nil {
		return failResult("读取域 UUID 失败: " + err.Error()), nil
	}
	if strings.TrimSpace(uuid) == "" {
		return failResult("节点未返回域 UUID"), nil
	}
	return &agent.Result{
		Success: true,
		Message: "节点已登记该域",
		Data:    map[string]any{agent.VMAdoptDataKey: agent.VMAdoptInfo{UUID: strings.TrimSpace(uuid)}},
	}, nil
}

// vmXMLApply 实现 vm.xml.apply：validate 只校验，define 才落盘。
func vmXMLApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	xmlText := strParam(op.Params, "xml")
	if strings.TrimSpace(xmlText) == "" {
		return failResult("域定义为空"), nil
	}
	// 只允许定义当前目标域：一份误贴的 XML 能把另一台机器整份覆盖掉。
	if name := elementText(xmlText, "name"); name != "" && name != op.Target {
		return failResult(fmt.Sprintf("域定义里的名称 %q 与目标 %q 不一致", name, op.Target)), nil
	}

	path, cleanup, err := writeTempXML("xml-"+op.Target, xmlText)
	if err != nil {
		return failResult("写入临时定义失败: " + err.Error()), nil
	}
	defer cleanup()

	switch action {
	case "validate":
		report(Stage{Key: "validate", Name: "校验域定义", Index: 1, Total: 1})
		info := agent.VMXMLValidateInfo{Valid: true}
		if err := sys.Run0(ctx, "virt-xml-validate", path, "domain"); err != nil {
			if isMissingExecutable(err) {
				// 精简安装可能没有 virt-xml-validate；此时不做 schema 校验，
				// 但必须明确告诉用户，而不是假装校验通过。
				info.Warnings = []string{
					"节点缺少 virt-xml-validate，未做 schema 校验；保存时 libvirt 仍会拒绝非法定义",
				}
			} else {
				info.Valid = false
				info.Errors = []string{err.Error()}
			}
		}
		return &agent.Result{
			Success: true,
			Data:    map[string]any{agent.VMXMLValidateKey: info},
		}, nil

	case "define":
		report(Stage{Key: "define", Name: "写入域定义", Index: 1, Total: 1})
		if _, err := virshWrite(ctx, "define", path); err != nil {
			return failResult("libvirt 拒绝了这份定义: " + err.Error()), nil
		}
		msg := "域定义已更新"
		if running, err := vmRunning(ctx, op.Target); err == nil && running {
			msg += "；运行中的域需要重启后才会应用新定义"
		}
		return &agent.Result{Success: true, Message: msg}, nil

	default:
		return failResult("未知的 XML 动作: " + action), nil
	}
}

// isMissingExecutable 判断命令不存在（而不是命令执行失败）。
func isMissingExecutable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "executable file not found") ||
		strings.Contains(s, "no such file or directory")
}

// vmDiskResize 实现 vm.disk.resize：关机态扩系统盘。
func vmDiskResize(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	oldGB, _ := paramInt(op.Params, "old_gb")
	newGB, ok := paramInt(op.Params, "new_gb")
	if !ok || newGB <= 0 {
		return failResult("缺少有效的新容量"), nil
	}

	report(Stage{Key: "check_shrinking", Name: "确认是扩容而非缩容", Index: 1, Total: 3})
	if oldGB > 0 && newGB <= oldGB {
		return failResult(fmt.Sprintf("只能扩容：当前 %d GB，目标 %d GB", oldGB, newGB)), nil
	}
	running, err := vmRunning(ctx, op.Target)
	if err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	}
	if running {
		return failResult("虚拟机正在运行：请先关机，或在来宾自动化里使用「扩容磁盘」"), nil
	}

	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	source := ""
	for _, d := range sys.ParseDomainDisks(xmlText) {
		if d.IsCDROM || d.IsFloppy {
			continue
		}
		if d.Dev == "vda" || d.Dev == "sda" {
			source = d.SourceFile
			break
		}
	}
	if source == "" {
		return failResult("找不到系统盘（vda / sda）"), nil
	}

	report(Stage{Key: "grow_image", Name: "扩大镜像文件", Index: 2, Total: 3})
	if _, err := sys.RunTimeout(ctx, 30*time.Minute, "qemu-img", "resize", source, fmt.Sprintf("%dG", newGB)); err != nil {
		return failResult("qemu-img resize 失败: " + err.Error()), nil
	}
	size, err := sys.QemuImgSize(ctx, source)
	if err != nil {
		return failResult("扩容后无法读取镜像容量: " + err.Error()), nil
	}
	const gib = int64(1024 * 1024 * 1024)
	if size < int64(newGB)*gib {
		return failResult(fmt.Sprintf("扩容未生效：镜像仍为 %.1f GB", float64(size)/float64(gib))), nil
	}

	report(Stage{Key: "update_domain", Name: "更新域定义", Index: 3, Total: 3})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskResizeInfo{
			Applied: true, OldGB: oldGB, NewGB: newGB,
			GuestGrowNeeded: true,
			GuestGrowHint: "在来宾里用 growpart /dev/vda 1 扩分区，再 resize2fs /dev/vda1 扩文件系统" +
				"（Windows 用「磁盘管理」的「扩展卷」）",
			Message: "宿主机侧已扩容",
		}},
	}, nil
}

// vmDisksIndependent 实现 vm.disks.independent：把链接克隆的 overlay 合并成独立镜像。
//
// 用 qemu-img convert 而不是 qemu-img commit：commit 是把 overlay 的内容
// **写回父盘**，会改坏模板；convert 才是读整条链、写一份独立文件。
func vmDisksIndependent(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	freedFrom := strParam(op.Params, "freed_from")

	report(Stage{Key: "verify_stopped", Name: "确认虚拟机已停机", Index: 1, Total: 3})
	running, err := vmRunning(ctx, op.Target)
	if err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	}
	if running {
		return failResult("需要先关机：合并要复制整条镜像链，运行中的写入会让结果与任何时刻都对不上"), nil
	}
	if snap, err := hasSnapshots(ctx, op.Target); err != nil {
		return failResult("无法确认快照状态，已中止: " + err.Error()), nil
	} else if snap {
		return failResult("该虚拟机存在快照，合并磁盘会丢失快照链；请先删除快照再操作"), nil
	}

	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}

	var merged int
	var sizeBytes int64
	for _, d := range sys.ParseDomainDisks(xmlText) {
		if d.IsCDROM || d.IsFloppy || d.SourceFile == "" {
			continue
		}
		backing, err := qemuImgBacking(ctx, d.SourceFile)
		if err != nil {
			return failResult(fmt.Sprintf("读取 %s 的 backing 信息失败: %v", d.Dev, err)), nil
		}
		if backing == "" {
			continue
		}

		report(Stage{
			Key: "merge_backing", Name: "合并底层镜像",
			Index: 2, Total: 3,
		})
		tmp := d.SourceFile + ".kc-independent-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		_ = os.Remove(tmp)
		if _, err := sys.RunTimeout(ctx, qemuConvertTimeout,
			"qemu-img", "convert", "-O", "qcow2", d.SourceFile, tmp); err != nil {
			_ = os.Remove(tmp)
			return failResult(fmt.Sprintf("合并 %s 失败: %v", d.Dev, err)), nil
		}
		// 转换结果必须确实没有父盘，否则"独立"只是一个标记。
		if backing, err := qemuImgBacking(ctx, tmp); err != nil || backing != "" {
			_ = os.Remove(tmp)
			return failResult(fmt.Sprintf("合并后的 %s 仍依赖父盘（%s）", d.Dev, backing)), nil
		}
		if err := ensureQemuReadable(tmp); err != nil {
			_ = os.Remove(tmp)
			return failResult("设置合并镜像的属主失败: " + err.Error()), nil
		}
		if err := os.Rename(tmp, d.SourceFile); err != nil {
			_ = os.Remove(tmp)
			return failResult(fmt.Sprintf("替换 %s 失败: %v", d.Dev, err)), nil
		}
		if st, err := os.Stat(d.SourceFile); err == nil {
			sizeBytes += st.Size()
		}
		merged++
	}

	report(Stage{Key: "verify_standalone", Name: "确认不再依赖父盘", Index: 3, Total: 3})
	msg := "磁盘已合并为独立镜像"
	if merged == 0 {
		msg = "所有磁盘已经是独立镜像，无需合并"
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VMDiskIndependentKey: agent.VMDiskIndependentInfo{
			Applied: true, FreedFrom: freedFrom, SizeBytes: sizeBytes, Message: msg,
		}},
	}, nil
}

// qemuImgBacking 返回镜像的完整父盘路径；空字符串表示没有父盘。
func qemuImgBacking(ctx context.Context, path string) (string, error) {
	out, err := sys.Run(ctx, "qemu-img", "info", "--output=json", path)
	if err != nil {
		return "", err
	}
	var info struct {
		Backing     string `json:"backing-filename"`
		FullBacking string `json:"full-backing-filename"`
	}
	if err := jsonUnmarshalTo([]byte(out), &info); err != nil {
		return "", fmt.Errorf("qemu-img 输出解析失败: %w", err)
	}
	if info.FullBacking != "" {
		return info.FullBacking, nil
	}
	return info.Backing, nil
}

// hasSnapshots 报告域是否有快照。有快照时移动/合并磁盘会破坏快照链。
//
// 查询失败返回 error 而不是 false：无法确认时继续操作，可能把快照链
// 悄悄弄坏——那比直接拒绝更糟。
func hasSnapshots(ctx context.Context, vm string) (bool, error) {
	out, err := sys.Run(ctx, "virsh", "-c", "qemu:///system", "snapshot-list", "--name", vm)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// vmDiskChange 实现 vm.disk.change：attach / detach / bus / migrate。
func vmDiskChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	running, err := vmRunning(ctx, op.Target)
	if err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	}

	switch action {
	case agent.DiskActionAttach:
		return diskAttach(ctx, op, report, running)
	case agent.DiskActionDetach:
		return diskDetach(ctx, op, report, running)
	case agent.DiskActionBus:
		return diskChangeBus(ctx, op, report, running)
	case agent.DiskActionMigrate:
		return diskMigrate(ctx, op, report, running)
	default:
		return failResult("不支持的磁盘动作: " + action), nil
	}
}

// diskAttach 挂载一块来自“我的存储”的磁盘文件。
func diskAttach(ctx context.Context, op agent.Operation, report Reporter, running bool) (*agent.Result, error) {
	report(Stage{Key: "device_apply", Name: "应用磁盘配置", Index: 1, Total: 2})
	rel := strParam(op.Params, "rel_path")
	if rel == "" {
		return failResult("缺少 rel_path：控制面必须把文件解析为节点相对路径"), nil
	}
	if filepath.IsAbs(rel) {
		return failResult("rel_path 必须是相对于用户存储根的路径"), nil
	}
	source := filepath.Join(userdataRoot, rel)
	// Join 只做清洗不做约束：带 ../ 的 rel_path 会落到存储根之外。
	if source != userdataRoot && !strings.HasPrefix(source, userdataRoot+"/") {
		return failResult("磁盘路径越出用户存储根: " + source), nil
	}
	st, err := os.Stat(source)
	if err != nil {
		return failResult("磁盘文件不存在: " + err.Error()), nil
	}
	if st.IsDir() {
		return failResult("目标是目录，不是磁盘文件"), nil
	}
	if err := ensureQemuReadable(source); err != nil {
		return failResult("设置磁盘文件属主失败: " + err.Error()), nil
	}
	// qemu 要打开的镜像在 images 之外，AppArmor 需要放行（与光驱/快照同根因）。
	ensureAppArmorAllowsImages(userdataRoot)

	bus := strParam(op.Params, "bus")
	if bus == "" {
		bus = "virtio"
	}
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	// q35 槽位耗尽时控制面会降级到 scsi；纯 virtio 的域没有 SCSI 控制器，
	// 必须先补上。运行中无法热插拔控制器，只能要求关机。
	if bus == "scsi" && !hasSCSIController(xmlText) {
		if running {
			return failResult("该虚拟机没有 virtio-scsi 控制器，运行中无法添加；请关机后再挂载"), nil
		}
		xmlText = addSCSIController(xmlText)
		if err := defineXML(ctx, xmlText); err != nil {
			return failResult("添加 virtio-scsi 控制器失败: " + err.Error()), nil
		}
	}
	dev := nextFreeDevForBus(xmlText, bus)
	frag := diskFragment(dev, bus, source, "qcow2")
	path, cleanup, err := writeTempXML("disk-"+op.Target, frag)
	if err != nil {
		return failResult("写入磁盘定义失败: " + err.Error()), nil
	}
	defer cleanup()

	reboot := false
	args := []string{"attach-device"}
	if running {
		args = append(args, "--live", "--config")
	} else {
		args = append(args, "--config")
	}
	args = append(args, op.Target, path)
	if _, err := virshWrite(ctx, args...); err != nil {
		if running {
			// 热插拔失败（槽位/机型）时退到持久配置：盘不会立刻出现，
			// 但重启后可用。必须把"要重启"明确回报，不能当成功糊过去。
			if _, err2 := virshWrite(ctx, "attach-device", "--config", op.Target, path); err2 != nil {
				return failResult("挂载磁盘失败: " + err.Error()), nil
			}
			reboot = true
		} else {
			return failResult("挂载磁盘失败: " + err.Error()), nil
		}
	}

	report(Stage{Key: "guest_check", Name: "检查来宾是否需要重启", Index: 2, Total: 2})
	if !diskExists(ctx, op.Target, dev) {
		return failResult("libvirt 接受了挂载命令，但域定义里没有新设备 " + dev), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskChangeInfo{
			Dev: dev, RebootNeeded: reboot, GuestRefreshNeeded: true,
			Message: "磁盘已挂载",
		}},
	}, nil
}

// diskDetach 卸载一块数据盘。设备已不存在时视为成功（幂等）。
func diskDetach(ctx context.Context, op agent.Operation, report Reporter, running bool) (*agent.Result, error) {
	report(Stage{Key: "device_apply", Name: "应用磁盘配置", Index: 1, Total: 2})
	dev := strParam(op.Params, "dev")
	if dev == "" {
		return failResult("缺少设备名"), nil
	}
	if sysIsSystemDev(dev) {
		return failResult("系统盘不可卸载"), nil
	}
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	if !diskExistsInXML(xmlText, dev) {
		return &agent.Result{
			Success: true,
			Message: "设备已不存在（幂等成功）",
			Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskChangeInfo{
				Dev: dev, GuestRefreshNeeded: true, Message: "设备已不存在",
			}},
		}, nil
	}

	reboot := false
	args := []string{"detach-disk"}
	if running {
		args = append(args, "--live", "--config")
	} else {
		args = append(args, "--config")
	}
	args = append(args, op.Target, dev)
	if _, err := virshWrite(ctx, args...); err != nil {
		if running {
			if _, err2 := virshWrite(ctx, "detach-disk", "--config", op.Target, dev); err2 != nil {
				return failResult("卸载磁盘失败: " + err.Error()), nil
			}
			reboot = true
		} else {
			return failResult("卸载磁盘失败: " + err.Error()), nil
		}
	}

	report(Stage{Key: "guest_check", Name: "检查来宾是否需要重启", Index: 2, Total: 2})
	if diskExists(ctx, op.Target, dev) && !reboot {
		return failResult("libvirt 接受了卸载命令，但域定义里仍有设备 " + dev), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskChangeInfo{
			Dev: dev, RebootNeeded: reboot, GuestRefreshNeeded: true, Message: "磁盘已卸载",
		}},
	}, nil
}

// diskChangeBus 换磁盘总线（必须关机）。
func diskChangeBus(ctx context.Context, op agent.Operation, report Reporter, running bool) (*agent.Result, error) {
	report(Stage{Key: "device_apply", Name: "应用磁盘配置", Index: 1, Total: 2})
	dev := strParam(op.Params, "dev")
	bus := strParam(op.Params, "bus")
	if dev == "" || bus == "" {
		return failResult("缺少设备名或总线类型"), nil
	}
	if running {
		return failResult("换总线需要关机：来宾里的设备路径会变"), nil
	}
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	newXML, err := setDiskBus(xmlText, dev, bus)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if err := defineXML(ctx, newXML); err != nil {
		return failResult("写入域定义失败: " + err.Error()), nil
	}

	report(Stage{Key: "guest_check", Name: "检查来宾是否需要重启", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskChangeInfo{
			Dev: dev, RebootNeeded: true, Message: "磁盘总线已改为 " + bus,
		}},
	}, nil
}

// diskMigrate 把一块磁盘搬到另一个存储池。
//
// 离线：复制/移动文件后改域定义；热迁移：blockcopy + pivot。两条路径都先
// 拒绝有快照的域——移动当前盘会让外部快照的链指向不存在的路径。
func diskMigrate(ctx context.Context, op agent.Operation, report Reporter, running bool) (*agent.Result, error) {
	report(Stage{Key: "device_apply", Name: "应用磁盘配置", Index: 1, Total: 2})
	dev := strParam(op.Params, "dev")
	targetPath := strParam(op.Params, "target_path")
	if dev == "" || targetPath == "" {
		return failResult("缺少设备名或目标存储池路径"), nil
	}
	if running && !boolParam(op.Params, "hot") {
		return failResult("虚拟机正在运行：热迁移需要显式允许，否则请先关机"), nil
	}
	if snap, err := hasSnapshots(ctx, op.Target); err != nil {
		return failResult("无法确认快照状态，已中止: " + err.Error()), nil
	} else if snap {
		return failResult("该虚拟机存在快照，迁移磁盘会破坏快照链；请先删除快照"), nil
	}
	source := sysDiskSourceOf(ctx, op.Target, dev)
	if source == "" {
		return failResult("找不到设备 " + dev + " 的镜像路径"), nil
	}
	if st, err := os.Stat(targetPath); err != nil || !st.IsDir() {
		return failResult("目标存储池路径不可用: " + targetPath), nil
	}
	dst := filepath.Join(targetPath, filepath.Base(source))
	if source == dst {
		return &agent.Result{
			Success: true,
			Message: "磁盘已位于目标存储池",
			Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskChangeInfo{
				Dev: dev, Message: "磁盘已位于目标存储池",
			}},
		}, nil
	}
	if _, err := os.Stat(dst); err == nil {
		return failResult("目标位置已存在同名文件: " + dst), nil
	}

	if running {
		if _, err := sys.RunTimeout(ctx, blockCopyTimeout,
			"virsh", "-c", "qemu:///system", "blockcopy", op.Target, dev, dst,
			"--format=qcow2", "--wait", "--verbose"); err != nil {
			return failResult("blockcopy 失败: " + err.Error()), nil
		}
		if _, err := virshWrite(ctx, "blockjob", op.Target, dev, "--pivot"); err != nil {
			return failResult("blockcopy 完成但 pivot 失败: " + err.Error()), nil
		}
	} else {
		if err := moveFile(ctx, source, dst); err != nil {
			return failResult("移动磁盘文件失败: " + err.Error()), nil
		}
	}

	xmlText, err := sys.DumpXML(ctx, op.Target, running)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	newXML, err := setDiskSource(xmlText, dev, dst)
	if err != nil {
		if !running {
			_ = moveFile(context.Background(), dst, source)
		}
		return failResult("更新域定义失败: " + err.Error()), nil
	}
	if err := defineXML(ctx, newXML); err != nil {
		if !running {
			_ = moveFile(context.Background(), dst, source)
		}
		return failResult("写入域定义失败: " + err.Error()), nil
	}
	// 热迁移在 pivot 之后源文件已经不再被使用；离线移动时源已经不存在。
	if running {
		_ = os.Remove(source)
	}

	report(Stage{Key: "guest_check", Name: "检查来宾是否需要重启", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.VMDiskDataKey: agent.VMDiskChangeInfo{
			Dev: dev, Message: "磁盘已迁移到 " + filepath.Dir(dst),
		}},
	}, nil
}

// vmInterfaceChange 实现 vm.interface.change：add / update / remove。
//
// 控制面的 add 与 update 共用同一个 action（update）：节点按 MAC 判断
// "已存在则替换、不存在则挂载"，因此重试天然幂等。
func vmInterfaceChange(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	mac := strParam(op.Params, "mac")
	model := strParam(op.Params, "model")
	bridge := strParam(op.Params, "bridge_name")
	rateMbps, _ := paramInt(op.Params, "rate_limit_mbps")

	running, err := vmRunning(ctx, op.Target)
	if err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	}
	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	existing, found := interfaceBlockOf(xmlText, mac)

	report(Stage{Key: "config_write", Name: "更新域配置", Index: 1, Total: 2})

	switch action {
	case "remove":
		if !found {
			return &agent.Result{Success: true, Message: "网卡已不存在（幂等成功）"}, nil
		}
		path, cleanup, err := writeTempXML("nic-"+op.Target, existing)
		if err != nil {
			return failResult("写入网卡定义失败: " + err.Error()), nil
		}
		defer cleanup()
		args := []string{"detach-device"}
		if running {
			args = append(args, "--live", "--config")
		} else {
			args = append(args, "--config")
		}
		args = append(args, op.Target, path)
		if _, err := virshWrite(ctx, args...); err != nil {
			if running {
				if _, err2 := virshWrite(ctx, "detach-device", "--config", op.Target, path); err2 != nil {
					return failResult("移除网卡失败: " + err.Error()), nil
				}
			} else {
				return failResult("移除网卡失败: " + err.Error()), nil
			}
		}
		report(Stage{Key: "nic_hotplug", Name: "热插拔网卡", Index: 2, Total: 2})
		return &agent.Result{Success: true, Message: "网卡已移除"}, nil

	case "update", "add":
		frag := ifaceFragment(model, mac, bridge, rateMbps)
		path, cleanup, err := writeTempXML("nic-"+op.Target, frag)
		if err != nil {
			return failResult("写入网卡定义失败: " + err.Error()), nil
		}
		defer cleanup()

		reboot := false
		if running {
			// 运行中替换：先摘旧设备再挂新设备。update-device 在改 model/
			// 带宽时并不可靠，而 detach+attach 的语义是明确的。
			replacedByUpdate := false
			if found {
				oldPath, oldCleanup, err := writeTempXML("nic-old-"+op.Target, existing)
				if err != nil {
					return failResult("写入旧网卡定义失败: " + err.Error()), nil
				}
				if _, derr := virshWrite(ctx, "detach-device", "--live", "--config", op.Target, oldPath); derr != nil {
					// 摘不掉时退到 update-device；成功则旧网卡仍在且已被替换。
					if _, uerr := virshWrite(ctx, "update-device", "--live", op.Target, path); uerr != nil {
						oldCleanup()
						return failResult("更新网卡失败: " + derr.Error()), nil
					}
					replacedByUpdate = true
				}
				oldCleanup()
			}
			if !replacedByUpdate {
				if _, err := virshWrite(ctx, "attach-device", "--live", "--config", op.Target, path); err != nil {
					if _, err2 := virshWrite(ctx, "attach-device", "--config", op.Target, path); err2 != nil {
						// 旧网卡已经摘掉，新网卡又挂不上：把旧定义挂回去，
						// 别把用户的虚拟机留在“没有网卡”的状态。
						if found {
							if oldPath, c, werr := writeTempXML("nic-restore-"+op.Target, existing); werr == nil {
								_, _ = virshWrite(ctx, "attach-device", "--live", "--config", op.Target, oldPath)
								c()
							}
						}
						return failResult("挂载网卡失败: " + err.Error()), nil
					}
					reboot = true
				}
			}
		} else if found {
			if _, err := virshWrite(ctx, "update-device", "--config", op.Target, path); err != nil {
				// 旧定义不兼容时退到 detach+attach；仍失败才报错。
				oldPath, oldCleanup, werr := writeTempXML("nic-old-"+op.Target, existing)
				if werr != nil {
					return failResult("更新网卡失败: " + err.Error()), nil
				}
				defer oldCleanup()
				_, _ = virshWrite(ctx, "detach-device", "--config", op.Target, oldPath)
				if _, err2 := virshWrite(ctx, "attach-device", "--config", op.Target, path); err2 != nil {
					// 新定义挂不上：把旧定义挂回去。
					_, _ = virshWrite(ctx, "attach-device", "--config", op.Target, oldPath)
					return failResult("更新网卡失败: " + err.Error()), nil
				}
			}
		} else {
			if _, err := virshWrite(ctx, "attach-device", "--config", op.Target, path); err != nil {
				return failResult("挂载网卡失败: " + err.Error()), nil
			}
		}

		// 限速以 domiftune 为准（XML 里的 bandwidth 在某些驱动上不生效）。
		if mac != "" {
			tune := []string{"domiftune", op.Target, mac, "--config"}
			if rateMbps > 0 {
				tune = append(tune, "--inbound", fmt.Sprintf("average=%d", rateMbps*1000))
			}
			_, _ = virshWrite(ctx, tune...)
		}

		report(Stage{Key: "nic_hotplug", Name: "热插拔网卡", Index: 2, Total: 2})
		msg := "网卡配置已更新"
		if !found {
			msg = "网卡已挂载"
		}
		if reboot {
			msg += "（热插拔失败，已写入持久配置，重启后生效）"
		}
		return &agent.Result{Success: true, Message: msg}, nil

	default:
		return failResult("未知的网卡动作: " + action), nil
	}
}

// diskExists 报告持久定义里是否有指定设备。
func diskExists(ctx context.Context, vm, dev string) bool {
	xmlText, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return false
	}
	return diskExistsInXML(xmlText, dev)
}

// diskExistsInXML 报告一份域定义里是否有指定设备。
func diskExistsInXML(xmlText, dev string) bool {
	for _, d := range sys.ParseDomainDisks(xmlText) {
		if d.Dev == dev {
			return true
		}
	}
	return false
}

// moveFile 先尝试 rename（同一文件系统内是原子的），跨设备时退到复制后删除。
func moveFile(ctx context.Context, src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(ctx, src, dst); err != nil {
		return err
	}
	if err := os.Remove(src); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

// copyFile 带 ctx 取消的流式复制，避免几十 GB 的磁盘复制无法被任务取消。
func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()

	buf := make([]byte, 4<<20)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}
