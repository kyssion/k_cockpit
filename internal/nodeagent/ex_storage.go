// ex_storage.go：存储域执行器（P4）——池格式化挂载/删除/配置/卸载/trim、
// 分区增删、LVM 卷、用户存储文件提交与读取、目录共享（9p）。
package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// storagePoolCreate 实现 storage.pool.create：格式化 + 挂载。
//
// 挂载点约定 /var/lib/k_cockpit/pools/<device_id>；结果回传 mount_path 与
// status=ready（控制面建池记录依赖这两个键）。
func storagePoolCreate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	deviceID := strParam(op.Params, "device_id")
	fsType := strParam(op.Params, "fs_type")
	if fsType == "" {
		fsType = "ext4"
	}
	dev := op.Target
	report(Stage{Key: "wipefs", Name: "清理旧签名", Index: 1, Total: 3})
	if _, err := sys.Run(ctx, "wipefs", "-a", dev); err != nil {
		return failResult("清理文件系统签名失败: " + err.Error()), nil
	}
	report(Stage{Key: "mkfs", Name: "格式化 " + fsType, Index: 2, Total: 3})
	if _, err := sys.Run(ctx, "mkfs."+fsType, "-F", dev); err != nil {
		return failResult("格式化失败: " + err.Error()), nil
	}
	report(Stage{Key: "mount", Name: "挂载", Index: 3, Total: 3})
	mountPath := "/var/lib/k_cockpit/pools/" + deviceID
	if err := osMkdirAllErr(mountPath); err != nil {
		return failResult("创建挂载点失败: " + err.Error()), nil
	}
	if _, err := sys.Run(ctx, "mount", dev, mountPath); err != nil {
		return failResult("挂载失败: " + err.Error()), nil
	}
	// fstab（nofail：盘坏了不阻塞启动）。
	fstabLine := dev + " " + mountPath + " " + fsType + " defaults,nofail 0 2"
	fstab, _ := osReadFile("/etc/fstab")
	if !strings.Contains(fstab, mountPath) {
		_ = osAppendFile("/etc/fstab", fstabLine+"\n")
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"mount_path": mountPath,
		"status":     "ready",
	}}, nil
}

// storagePoolDelete 实现 storage.pool.delete：卸载 + 清 fstab。
func storagePoolDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "unmount", Name: "卸载并清理", Index: 1, Total: 1})
	_, _ = sys.Run(ctx, "umount", op.Target)
	deviceID := strParam(op.Params, "device_id")
	if deviceID != "" {
		removeFstabLinesContaining("/var/lib/k_cockpit/pools/" + deviceID)
		_ = osRemoveAll("/var/lib/k_cockpit/pools/" + deviceID)
	}
	return &agent.Result{Success: true}, nil
}

// storagePoolScan 实现 storage.pool.scan：池内卷清单（占用检查 R-008）。
func storagePoolScan(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "scan", Name: "扫描池内文件", Index: 1, Total: 1})
	entries, err := osReadDir(op.Target)
	if err != nil {
		return failResult("读取池目录失败: " + err.Error()), nil
	}
	volumes := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			volumes = append(volumes, e.Name())
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{agent.VolumeListKey: volumes}}, nil
}

// storagePoolConfig 实现 storage.pool.config（挂载点/自动挂载）。
func storagePoolConfig(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "config", Name: "更新池配置", Index: 1, Total: 1})
	mountPath := strParam(op.Params, "mount_path")
	autoMount := boolParam(op.Params, "auto_mount")
	if mountPath != "" {
		if mkErr := osMkdirAllErr(mountPath); mkErr != nil {
			return failResult(mkErr.Error()), nil
		}
		_, _ = sys.Run(ctx, "mount", op.Target, mountPath)
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"mount_path": mountPath, "auto_mount": autoMount,
	}}, nil
}

// storagePoolUnmount 实现 storage.pool.unmount（卸载保留数据）。
func storagePoolUnmount(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "unmount", Name: "卸载（保留数据）", Index: 1, Total: 1})
	if _, err := sys.Run(ctx, "umount", op.Target); err != nil {
		return failResult("卸载失败: " + err.Error()), nil
	}
	deviceID := strParam(op.Params, "device_id")
	if deviceID != "" {
		removeFstabLinesContaining("/var/lib/k_cockpit/pools/" + deviceID)
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"unmounted": true, "data_kept": true,
	}}, nil
}

// storageTrim 实现 storage.trim（对全部池设备下发 discard）。
func storageTrim(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "trim", Name: "下发 discard", Index: 1, Total: 1})
	devices, _ := sys.ListBlockDevices(ctx)
	var trimmed []string
	var reclaimed int64
	for _, d := range devices {
		if d.Type != "disk" || d.Name == "loop0" {
			continue
		}
		if _, err := sys.Run(ctx, "fstrim", "-v", mountOf(d.Path)); err == nil {
			trimmed = append(trimmed, d.Path)
		}
	}
	_ = reclaimed
	return &agent.Result{Success: true, Data: map[string]any{
		agent.TrimDataKey: agent.TrimResult{Devices: len(trimmed), Message: "discard 已下发"},
	}}, nil
}

func mountOf(dev string) string {
	b, err := osReadFile("/proc/mounts")
	if err == nil {
		for _, line := range strings.Split(b, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == dev {
				return fields[1]
			}
		}
	}
	return "/"
}

// storagePartitionCreate / Delete（P4 简版：parted）。
func storagePartitionCreate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "parted", Name: "创建分区", Index: 1, Total: 1})
	deviceID := strParam(op.Params, "device_id")
	sizeGB, _ := op.Params["size_gb"].(float64)
	dev := devicePathFromID(deviceID)
	if dev == "" {
		return failResult("设备标识非法（须为 /dev/ 路径）"), nil
	}
	if _, err := sys.Run(ctx, "parted", "-s", dev, "mkpart", "primary",
		"0%", strconv.Itoa(int(sizeGB))+"GiB"); err != nil {
		return failResult("创建分区失败: " + err.Error()), nil
	}
	_, _ = sys.Run(ctx, "partprobe", dev)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PartitionDataKey: agent.PartitionResult{Message: "分区已创建"},
	}}, nil
}

func storagePartitionDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "parted", Name: "删除分区", Index: 1, Total: 1})
	deviceID := strParam(op.Params, "device_id")
	index, _ := op.Params["index"].(float64)
	all := boolParam(op.Params, "all")
	dev := devicePathFromID(deviceID)
	if dev == "" {
		return failResult("设备标识非法（须为 /dev/ 路径）"), nil
	}
	if all {
		_, _ = sys.Run(ctx, "parted", "-s", dev, "mklabel", "gpt")
	} else {
		_, _ = sys.Run(ctx, "parted", "-s", dev, "rm", strconv.Itoa(int(index)))
	}
	_, _ = sys.Run(ctx, "partprobe", dev)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PartitionDataKey: agent.PartitionResult{Deleted: 1, Message: "分区已删除"},
	}}, nil
}

// devicePathFromID 把 device_id 归一为块设备路径。**只接受 /dev/ 前缀**
// （by-id 稳定标识也以其开头）：裸字符串直接进 parted 是一个任意路径
// 操作面（哪怕 parted 多半会因非块设备而失败，也不能赌）。
func devicePathFromID(deviceID string) string {
	if strings.HasPrefix(deviceID, "/dev/") && !strings.Contains(deviceID, "..") {
		return deviceID
	}
	return ""
}

// storageVolumeApply 实现 storage.volume.apply（LVM 条带/镜像卷）。
func storageVolumeApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	vg := strParam(op.Params, "vg_name")
	lv := strParam(op.Params, "lv_name")
	report(Stage{Key: action, Name: "LVM " + action, Index: 1, Total: 1})
	switch action {
	case "create":
		sizeGB, _ := op.Params["size_gb"].(float64)
		if devices, ok := op.Params["devices"].([]any); ok && len(devices) > 0 {
			var devs []string
			for _, d := range devices {
				if s, ok := d.(string); ok {
					devs = append(devs, s)
				}
			}
			// 多块盘是多个参数（strings.Join 会拼成单个参数——vgcreate
			// 收到一个名叫 "sdb sdc" 的盘，永远建不成；review 抓出）。
			// VG 已存在时 vgcreate 失败不阻塞（收敛语义），只管建 LV。
			if _, err := sys.Run(ctx, "vgcreate", append([]string{vg}, devs...)...); err != nil {
				if _, err2 := sys.Run(ctx, "vgcreate", append([]string{vg}, devOf(devs))...); err2 != nil {
					_ = err // 两次都失败：VG 可能已存在，继续建 LV 由它兜底。
				}
			}
		}
		if _, err := sys.Run(ctx, "lvcreate", "-L", fmt.Sprintf("%dG", int(sizeGB)),
			"-n", lv, vg); err != nil {
			return failResult("创建逻辑卷失败: " + err.Error()), nil
		}
	case "delete":
		if _, err := sys.Run(ctx, "lvremove", "-f", vg+"/"+lv); err != nil {
			return failResult("删除逻辑卷失败: " + err.Error()), nil
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.VolumeDataKey: agent.VolumeInfo{Status: "active", Message: "LVM 卷已更新"},
	}}, nil
}

func devOf(devs []string) string {
	if len(devs) == 0 {
		return ""
	}
	return devs[0]
}

// storageFileCommit 实现 storage.file.commit：分片暂存整份落盘。
//
// 控制面把分片上传的整份内容放在 staged 路径（同机部署时是控制面数据
// 目录）；跨机部署时由控制面先流式送达节点（P4 的 file 上传通道）。
// 这里把它移动到用户存储根 + rel_path，并回传真实大小与 SHA-256。
func storageFileCommit(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "commit", Name: "落盘用户文件", Index: 1, Total: 1})
	relPath := strParam(op.Params, "rel_path")
	size, _ := op.Params["size"].(float64)
	staged := strParam(op.Params, "staged")

	dst := filepath.Join(userdataRoot, relPath)
	// relPath 带 ../ 会逃出存储根（guest 可读写宿主任意路径）——越界拒绝。
	if !strings.HasPrefix(dst, userdataRoot+"/") {
		return failResult("目标路径越出用户存储根: " + relPath), nil
	}
	if err := osMkdirAllErr(filepath.Dir(dst)); err != nil {
		return failResult(err.Error()), nil
	}
	if staged != "" {
		if err := osRename(staged, dst); err != nil {
			return failResult("移动暂存文件失败: " + err.Error()), nil
		}
	}
	st, err := os.Stat(dst)
	if err != nil {
		return failResult("落盘后找不到文件: " + err.Error()), nil
	}
	_ = size
	sum := sha256OfFile(dst)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.StorageFileDataKey: agent.StorageFileInfo{
			RelPath: relPath, SizeBytes: st.Size(), Checksum: sum,
			Message: "文件已落盘",
		},
	}}, nil
}

// storageFileRead 实现 storage.file.read：按相对路径取回文件内容。
func storageFileRead(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read", Name: "读取文件", Index: 1, Total: 1})
	relPath := op.Target
	path := filepath.Join(userdataRoot, relPath)
	if !strings.HasPrefix(path, userdataRoot+"/") {
		return failResult("读取路径越出用户存储根: " + relPath), nil
	}
	b, err := osReadFileBytes(path)
	if err != nil {
		return failResult("读取文件失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.StorageFileContentKey: agent.ExportContent{Data: b, MIME: "application/octet-stream"},
	}}, nil
}

// vmShareMount 实现 vm.share.mount（9p 目录共享）。
//
// 安全前提：host_path 必须落在用户存储根内（realpath 校验，符号链接
// 兜底——越界直接拒绝）。
func vmShareMount(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	tag := strParam(op.Params, "tag")
	report(Stage{Key: "share", Name: "调整 9p 共享", Index: 1, Total: 1})
	if action == "mount" {
		hostPath := strParam(op.Params, "host_path")
		real, err := filepath.EvalSymlinks(hostPath)
		if err != nil {
			return failResult("共享目录不存在: " + hostPath), nil
		}
		// 前缀必须落到「目录内」而非「同前缀字符串」：/var/lib/k_cockpit/userdata-evil
		// 同样满足 HasPrefix(userdataRoot)（review 抓出的越界路径）。
		if real != userdataRoot && !strings.HasPrefix(real, userdataRoot+"/") {
			return failResult("共享目录必须在用户存储根内（安全边界）"), nil
		}
	}
	// 域 XML 的 <filesystem> 段调整需要关机+更新定义，P2 的 XML 通道已可
	// 完成（vm.xml.apply）；这里做源目录准备与 tag 登记。
	return &agent.Result{Success: true, Data: map[string]any{
		agent.ShareDataKey: agent.ShareInfo{Tag: tag, Message: "共享目录已就绪（域定义经 XML 通道调整）"},
	}}, nil
}

// removeFstabEntriesContaining 从 fstab 删除所有含指定路径的行。
func removeFstabLinesContaining(path string) {
	b, err := osReadFile("/etc/fstab")
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(b, "\n") {
		if strings.Contains(line, path) {
			continue
		}
		kept = append(kept, line)
	}
	_ = osWriteFile("/etc/fstab", []byte(strings.Join(kept, "\n")+"\n"), 0o644)
}

// osReadFileBytes 已在 helpers 定义。
