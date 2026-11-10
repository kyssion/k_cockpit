// ex_storage.go：存储域执行器（P4）——池格式化挂载/删除/配置/卸载/trim、
// 分区增删、LVM 卷、用户存储文件提交与读取、目录共享（9p）。
package nodeagent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// poolMountRoot 是存储池挂载点的根目录。
const poolMountRoot = "/var/lib/k_cockpit/pools"

// poolMountPath 拼接设备的挂载点。
//
// 用 filepath.Join 而不是字符串拼接：device_id 本身以 /dev/ 开头
// （或为分区名），直接拼接会得到 pools//dev/nbd0 这样的双斜杠路径
// （fstab 与面板展示里很难看，还让新旧路径的 fstab 清理对不上）。
func poolMountPath(deviceID string) string {
	return filepath.Join(poolMountRoot, deviceID)
}

// hostMountRun 在宿主机（PID 1）的挂载命名空间里执行挂载类命令。
//
// 为什么需要它：部署单元（deploy/agent/k_cockpit-agent.service）的
// ProtectSystem=full 让服务运行在独立挂载命名空间里，且传播方向为
// slave——服务内 mount 返回成功，宿主机与其他进程却看不到这个挂载，
// 服务重启后它随之消失（systemd 255 实机验证）。存储池的「格式化 +
// 挂载」于是表现为任务 success 但挂载不存在，虚拟机磁盘静默落到根
// 文件系统上。nsenter -t 1 -m 进入宿主命名空间执行，挂载对宿主机可见，
// 同时保留沙箱的只读保护。nsenter 不可用（极端精简环境）时回退为直接
// 执行——此时服务通常没有沙箱，行为等价于修复前。
func hostMountRun(ctx context.Context, name string, args ...string) (string, error) {
	if sys.RunOK(ctx, "nsenter", "-t", "1", "-m", "--", "true") {
		return sys.Run(ctx, "nsenter", append([]string{"-t", "1", "-m", "--", name}, args...)...)
	}
	return sys.Run(ctx, name, args...)
}

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
	mountPath := poolMountPath(deviceID)
	if err := osMkdirAllErr(mountPath); err != nil {
		return failResult("创建挂载点失败: " + err.Error()), nil
	}
	// 挂载必须落在宿主机命名空间（见 hostMountRun）。
	if _, err := hostMountRun(ctx, "mount", dev, mountPath); err != nil {
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
	_, _ = hostMountRun(ctx, "umount", op.Target)
	deviceID := strParam(op.Params, "device_id")
	if deviceID != "" {
		removeFstabLinesContaining(poolMountPath(deviceID))
		// 兼容旧版本（路径未用 Join，/dev 开头的 device_id 会产生双斜杠）
		// 写下的 fstab 行：升级后删除这类池时也要把它清掉。
		removeFstabLinesContaining(poolMountRoot + "/" + deviceID)
		_ = osRemoveAll(poolMountPath(deviceID))
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

// mountInfoPath 可在单测里替换，避免测试依赖宿主机的 /proc。
var mountInfoPath = "/proc/self/mountinfo"

// parseMountPoints 从 mountinfo 文本取第 5 列（挂载点）。
//
// mountinfo 用八进制转义空白与反斜杠，路径里出现空格时必须还原，
// 否则会被判成「未挂载」而误降级。
func parseMountPoints(content string) map[string]bool {
	repl := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	out := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		out[filepath.Clean(repl.Replace(fields[4]))] = true
	}
	return out
}

func readMountPoints() map[string]bool {
	data, err := os.ReadFile(mountInfoPath)
	if err != nil {
		// 读不到挂载表时返回空集合：安全失败方向是「没有池可用」，
		// 而不是猜一个容量。
		return nil
	}
	return parseMountPoints(string(data))
}

type statfsFunc func(path string, st *syscall.Statfs_t) error

// poolStats 只对**真实挂载点**返回容量。
//
// 节点重启后 nbd/loop 设备常常消失，但 fstab 条目和挂载目录还在；对这种
// 空目录做 statfs 会静默返回根文件系统的容量。此前控制面据此把 128G 系统盘
// 当成池容量写回，且每轮刷新把 last_reported_at 刷成最新，形成
// 「ready + 数据新鲜 + 有空间」的三重假象（第 13 轮真机对账）。
func poolStats(paths []string, mounts map[string]bool, statfs statfsFunc) []agent.PoolStat {
	stats := make([]agent.PoolStat, 0, len(paths))
	for _, path := range paths {
		// 越出挂载根的路径直接忽略：控制面参数不能变成任意路径探测面。
		if path != poolMountRoot && !strings.HasPrefix(path, poolMountRoot+"/") {
			continue
		}
		if !mounts[filepath.Clean(path)] {
			stats = append(stats, agent.PoolStat{Path: path, Mounted: false})
			continue
		}
		var st syscall.Statfs_t
		if err := statfs(path, &st); err != nil {
			stats = append(stats, agent.PoolStat{Path: path, Mounted: false})
			continue
		}
		stats = append(stats, agent.PoolStat{
			Path:       path,
			Mounted:    true,
			TotalBytes: int64(st.Blocks) * int64(st.Bsize),
			FreeBytes:  int64(st.Bavail) * int64(st.Bsize),
		})
	}
	return stats
}

// storagePoolStat 实现 storage.pool.stat：对给定挂载点批量 statfs。
//
// 路径必须落在存储池挂载根之内——来自控制面的参数不能变成任意路径的
// 探测面（路径语义硬规则）。单个路径失败只跳过自己：一个池被手动卸载
// 不该让其它池的容量也失真；全部成功的路径批量返回。
func storagePoolStat(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "statfs", Name: "读取池容量", Index: 1, Total: 1})
	raw, _ := op.Params["paths"].([]any)
	paths := make([]string, 0, len(raw))
	for _, p := range raw {
		if s, ok := p.(string); ok && s != "" {
			paths = append(paths, s)
		}
	}
	stats := poolStats(paths, readMountPoints(), syscall.Statfs)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PoolStatListKey: stats,
	}}, nil
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
		_, _ = hostMountRun(ctx, "mount", op.Target, mountPath)
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"mount_path": mountPath, "auto_mount": autoMount,
	}}, nil
}

// storagePoolUnmount 实现 storage.pool.unmount（卸载保留数据）。
func storagePoolUnmount(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "unmount", Name: "卸载（保留数据）", Index: 1, Total: 1})
	if _, err := hostMountRun(ctx, "umount", op.Target); err != nil {
		return failResult("卸载失败: " + err.Error()), nil
	}
	deviceID := strParam(op.Params, "device_id")
	if deviceID != "" {
		removeFstabLinesContaining(poolMountPath(deviceID))
		removeFstabLinesContaining(poolMountRoot + "/" + deviceID)
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
		devices, ok := op.Params["devices"].([]any)
		if !ok || len(devices) == 0 {
			return failResult("缺少物理设备"), nil
		}
		var devs []string
		for _, d := range devices {
			if s, ok := d.(string); ok && s != "" {
				devs = append(devs, s)
			}
		}
		if len(devs) == 0 {
			return failResult("缺少物理设备"), nil
		}
		// vgcreate 只接受**已经是 PV** 的设备。裸盘（以及建过池、留着 ext4
		// 签名的盘）必须先 pvcreate；过去这里把 vgcreate 的失败整段吞掉，
		// lvcreate 随后报 "Volume group not found"，创建卷在真机上从未成功。
		for _, d := range devs {
			if sys.RunOK(ctx, "pvdisplay", d) {
				continue
			}
			if _, err := sys.Run(ctx, "pvcreate", d); err != nil {
				// 残留文件系统签名时 pvcreate 会拒；清掉签名再试一次，
				// 两次都失败就如实报错，不再硬着头皮往下建 LV。
				_, _ = sys.Run(ctx, "wipefs", "-a", d)
				if _, err2 := sys.Run(ctx, "pvcreate", d); err2 != nil {
					return failResult("初始化物理卷失败: " + err2.Error()), nil
				}
			}
		}
		if !sys.RunOK(ctx, "vgdisplay", vg) {
			if _, err := sys.Run(ctx, "vgcreate", append([]string{vg}, devs...)...); err != nil {
				return failResult("创建卷组失败: " + err.Error()), nil
			}
		}
		if _, err := sys.Run(ctx, "lvcreate", "-L", fmt.Sprintf("%dG", int(sizeGB)),
			"-n", lv, vg); err != nil {
			return failResult("创建逻辑卷失败: " + err.Error()), nil
		}
	case "delete":
		_, lvErr := sys.Run(ctx, "lvremove", "-f", vg+"/"+lv)
		// 卷组根本不存在 = 卷从来没建起来（例如上一次创建失败留下的孤儿
		// 记录）。删除是幂等的，按「已经没了」处理；否则那条记录永远删不
		// 掉、设备也一直被它占着，用户点删除只会一直收到失败。
		if lvErr != nil && sys.RunOK(ctx, "lvdisplay", vg+"/"+lv) {
			return failResult("删除逻辑卷失败: " + lvErr.Error()), nil
		}
		// 卷组与物理卷都归这个卷所有：只删 LV 会把设备一直占着，
		// 下一次在这块盘上建卷会拿到「已属于另一个 VG」。
		_, _ = sys.Run(ctx, "vgremove", "-f", vg)
		if devices, ok := op.Params["devices"].([]any); ok {
			for _, d := range devices {
				if s, ok := d.(string); ok && s != "" {
					_, _ = sys.Run(ctx, "pvremove", s)
				}
			}
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.VolumeDataKey: agent.VolumeInfo{Status: "active", Message: "LVM 卷已更新"},
	}}, nil
}

// storageFilePush 实现 storage.file.push：接收控制面推送的内容分块。
//
// 分块按 offset 写进 `<最终路径>.kcpart` 暂存文件，等 commit 收口。
// rel_path 的越界校验与 commit 相同（落点必须留在用户存储根内）。
func storageFilePush(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "push", Name: "接收文件分块", Index: 1, Total: 1})
	relPath := strParam(op.Params, "rel_path")
	dataB64 := strParam(op.Params, "data")
	offset, _ := op.Params["offset"].(float64)
	if relPath == "" {
		return failResult("缺少 rel_path"), nil
	}
	raw, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return failResult("分块内容不是合法 base64: " + err.Error()), nil
	}
	dst := filepath.Join(userdataRoot(), relPath)
	if !strings.HasPrefix(dst, userdataRoot()+"/") {
		return failResult("目标路径越出用户存储根: " + relPath), nil
	}
	part := dst + ".kcpart"
	if err := osMkdirAllErr(filepath.Dir(part)); err != nil {
		return failResult("创建暂存目录失败: " + err.Error()), nil
	}
	f, err := osOpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return failResult("打开暂存文件失败: " + err.Error()), nil
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(raw, int64(offset)); err != nil {
		return failResult("写入分块失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true}, nil
}

// storageFileCommit 实现 storage.file.commit：把暂存内容落成正式文件。
//
// 三种来源按优先级：staged（同机部署下控制面与节点共享文件系统的兼容
// 路径）、link_from_rel（秒传：同一份内容登记成两条记录）、`.kcpart`
// 暂存文件（跨机部署：内容经 storage.file.push 分块送达）。落盘后回传
// 真实大小与 SHA-256，控制面据此核对传输完整性。
func storageFileCommit(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "commit", Name: "落盘用户文件", Index: 1, Total: 1})
	relPath := strParam(op.Params, "rel_path")
	size, _ := op.Params["size"].(float64)
	staged := strParam(op.Params, "staged")
	// link_from_rel 是「秒传」用到的来源相对路径（第 3 轮 DEF-R3-05）。
	//
	// 秒传的语义是"同一份内容登记成两条记录"，所以目标路径上**必须真的有
	// 一份文件**。旧实现只写了 DB 记录、没有建立任何文件，于是接口回
	// `instant=true` 与一个新 rel_path，而那个路径 `os.path.isfile=false`，
	// 之后按它取文件、装系统、导出全部失败。
	linkFrom := strParam(op.Params, "link_from_rel")

	dst := filepath.Join(userdataRoot(), relPath)
	// relPath 带 ../ 会逃出存储根（guest 可读写宿主任意路径）——越界拒绝。
	if !strings.HasPrefix(dst, userdataRoot()+"/") {
		return failResult("目标路径越出用户存储根: " + relPath), nil
	}
	if err := osMkdirAllErr(filepath.Dir(dst)); err != nil {
		return failResult(err.Error()), nil
	}
	switch {
	case linkFrom != "":
		// 来源可能是切换存储根之前上传的文件（在历史根里），因此走回退寻址。
		src := resolveExistingUserdataPath(linkFrom)
		if !underUserdataRoot(src) {
			return failResult("来源路径越出用户存储根: " + linkFrom), nil
		}
		if err := linkOrCopyFile(src, dst); err != nil {
			return failResult("秒传落地失败: " + err.Error()), nil
		}
	case staged != "":
		if err := osRename(staged, dst); err != nil {
			return failResult("移动暂存文件失败: " + err.Error()), nil
		}
	default:
		// 跨机部署：内容已由 storage.file.push 写进 <最终路径>.kcpart。
		part := dst + ".kcpart"
		if err := osRename(part, dst); err != nil {
			return failResult("暂存文件不存在或不可移动（分块推送未完成？）: " + err.Error()), nil
		}
	}
	st, err := os.Stat(dst)
	if err != nil {
		return failResult("落盘后找不到文件: " + err.Error()), nil
	}
	// 控制面声明的大小与实际落盘不一致，说明推送丢了块——当场判失败，
	// 不等控制面比对摘要（那条路径会让坏文件在节点上多活一会儿）。
	if size > 0 && st.Size() != int64(size) {
		return failResult(fmt.Sprintf("落盘大小不一致：声明 %d 字节，实际 %d 字节",
			int64(size), st.Size())), nil
	}
	sum := sha256OfFile(dst)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.StorageFileDataKey: agent.StorageFileInfo{
			RelPath: relPath, SizeBytes: st.Size(), Checksum: sum,
			Message: "文件已落盘",
		},
	}}, nil
}

// linkOrCopyFile 把 src 落到 dst：优先硬链接，跨设备/不支持时回退为复制。
//
// 为什么优先硬链接：秒传的全部意义是"不搬字节"。同一用户存储根内的两个
// 文件通常在同一文件系统上，硬链接是零拷贝且原子（要么有要么没有），
// 也不会出现"复制到一半失败留下半个文件"。
//
// 为什么必须有复制回退：存储根可能跨设备（bind mount、独立分区、overlay
// 与网络文件系统都不支持硬链接）。只有硬链接的话，这些环境上秒传会直接
// 失败——而用户看到的是"上传失败"，不是"不支持秒传"。
//
// **目标已存在时拒绝**：秒传不该悄悄覆盖同名文件（DB 唯一约束会拒绝这条
// 记录，若我们先覆盖了文件，等于把用户已有的内容替换掉而记录没变）。
func linkOrCopyFile(src, dst string) error {
	if _, err := osStat(dst); err == nil {
		return fmt.Errorf("目标位置已存在文件: %s", filepath.Base(dst))
	}
	if _, err := osStat(src); err != nil {
		return fmt.Errorf("来源文件不存在: %w", err)
	}
	if err := osLink(src, dst); err != nil {
		// 跨设备（EXDEV）或不支持硬链接的文件系统：回退为复制。
		log.Printf("[nodeagent] 硬链接失败，回退为复制 src=%s dst=%s: %v", src, dst, err)
		return copyFileSync(src, dst)
	}
	return nil
}

// resolveExistingUserdataPath 在**当前存储根**里找文件，找不到时回退到历史
// 默认根。
//
// 为什么要回退：存储根切到存储池之后，升级前上传的文件仍留在系统盘的
// /var/lib/k_cockpit/userdata 下。只按新根拼路径，它们会变成"列得出来、
// 下不下来、装不了系统"的记录——比丢文件更难解释。
//
// 只用于**读/秒传来源**这类既有内容的寻址；写入永远落在当前根（新文件
// 从一开始就在池里，不会再回到系统盘）。
func resolveExistingUserdataPath(rel string) string {
	p := filepath.Join(userdataRoot(), rel)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if !filepath.IsAbs(rel) && userdataRoot() != defaultUserdataRoot {
		old := filepath.Join(defaultUserdataRoot, rel)
		if _, err := os.Stat(old); err == nil {
			return old
		}
	}
	return p
}

// underUserdataRoot 判断路径是否落在当前或历史存储根之内。
func underUserdataRoot(p string) bool {
	for _, root := range []string{userdataRoot(), defaultUserdataRoot} {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

// storageFileRead 实现 storage.file.read：按相对路径取回文件内容。
func storageFileRead(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read", Name: "读取文件", Index: 1, Total: 1})
	relPath := op.Target
	path := resolveExistingUserdataPath(relPath)
	if !underUserdataRoot(path) {
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

// storageFileDelete 实现 storage.file.delete：删掉一份用户存储文件。
//
// 寻址与读取完全一致（当前根优先、回退历史根）：存储根切到存储池之前上传
// 的文件还在系统盘的旧目录里，只按当前根拼路径会"删不掉"它们——而那些
// 正是最容易被遗忘的孤儿。
//
// **文件不存在返回成功**：删除是幂等的，而"本来就不在"正是它的目标状态。
// 反过来（报"文件不存在"）会让控制面把一条记录永远留着，或者让用户看到
// 一个他无法解决的错误。
func storageFileDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "delete", Name: "删除文件", Index: 1, Total: 1})
	relPath := strings.TrimSpace(op.Target)
	if relPath == "" {
		return failResult("缺少要删除的文件路径"), nil
	}
	// 绝对路径直接拒绝：请求里的路径一律是"相对存储根"，放行绝对路径等于
	// 把"能删宿主机的哪个文件"交给调用方——与共享目录同一条安全边界。
	if filepath.IsAbs(relPath) {
		return failResult("删除路径必须是相对用户存储根的路径: " + relPath), nil
	}
	path := resolveExistingUserdataPath(relPath)
	if !underUserdataRoot(path) {
		return failResult("删除路径越出用户存储根: " + relPath), nil
	}

	info := agent.StorageFileDeleteInfo{RelPath: relPath, Root: filepath.Dir(path)}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return &agent.Result{Success: true, Message: "文件已不存在（幂等成功）",
				Data: map[string]any{agent.StorageFileDeleteKey: info}}, nil
		}
		return failResult("检查文件失败: " + err.Error()), nil
	}
	if err := osRemove(path); err != nil {
		return failResult("删除文件失败: " + err.Error()), nil
	}
	info.Deleted = true
	return &agent.Result{Success: true, Message: "文件已删除",
		Data: map[string]any{agent.StorageFileDeleteKey: info}}, nil
}

// vmShareMount 实现 vm.share.mount（9p 目录共享）。
//
// 安全前提：host_path 必须落在用户存储根内（realpath 校验，符号链接
// 兜底——越界直接拒绝）。
func vmShareMount(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	tag := strings.TrimSpace(strParam(op.Params, "tag"))
	report(Stage{Key: "share", Name: "调整 9p 共享", Index: 1, Total: 1})

	if action == "mount" {
		hostPath := strParam(op.Params, "host_path")
		real, err := filepath.EvalSymlinks(hostPath)
		if err != nil {
			return failResult("共享目录不存在: " + hostPath), nil
		}
		// 前缀必须落到「目录内」而非「同前缀字符串」：/var/lib/k_cockpit/userdata-evil
		// 同样满足 HasPrefix(userdataRoot)（review 抓出的越界路径）。
		if real != userdataRoot() && !strings.HasPrefix(real, userdataRoot()+"/") {
			return failResult("共享目录必须在用户存储根内（安全边界）"), nil
		}
		if tag == "" {
			return failResult("缺少共享标识（tag），无法写入域定义"), nil
		}
		return mountShare(ctx, op.Target, tag, real, op.Params)
	}

	// **卸载要求共享真的存在**（第 3 轮 DEF-R3-09）：原来无论 action 是什么
	// 都直接返回 success，于是"卸载一个从未挂载过的 tag"也是成功——用户无法
	// 分辨"已经卸掉"与"本来就没有"，而这两种情况该做的事完全不同。
	if tag == "" {
		return failResult("缺少共享标识（tag），无法确认要卸载哪一个"), nil
	}
	return unmountShare(ctx, op.Target, tag)
}

// mountShare 把共享写进域定义（9p VirtFS）。
//
// 此前这里只做路径校验就回成功，域里从来没有 <filesystem>——控制面据此把
// 记录标成 active，用户在来宾里找不到挂载点，而"任务成功"让他以为是自己
// 用错了 tag（实机复现：`virsh dumpxml --inactive` 里一个 <filesystem> 都
// 没有）。现在真的改定义，并如实说明生效时机。
func mountShare(
	ctx context.Context, vmName, tag, hostPath string, params map[string]any,
) (*agent.Result, error) {
	xmlText, err := sys.DumpXML(ctx, vmName, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}

	// 幂等：同一个 tag 指向同一个目录，重复挂载是安全的重复操作。
	// 但同名指向**别的目录**时必须报错——静默改成新目录会让已经在用旧目录的
	// 来宾在下一次开机后读到另一份数据，而界面上什么都不会变。
	if existing, ok := shareBlock(xmlText, tag); ok {
		if src := attributeOf(existing.text, "<source", "dir"); src == hostPath {
			return &agent.Result{Success: true, Data: map[string]any{
				agent.ShareDataKey: agent.ShareInfo{
					Tag:     tag,
					Message: "该共享已在域定义里，无需重复挂载",
				},
			}}, nil
		} else {
			return failResult("域定义里已有同名共享 " + tag + "（指向 " + src +
				"），请先卸载再挂载到新目录"), nil
		}
	}

	accessMode := strings.TrimSpace(strParam(params, "security_model"))
	if !validShareAccessMode(accessMode) {
		// 非法取值不写进定义：libvirt 对 accessmode 的枚举很严，写错会让
		// 整个 define 被拒，而报错在那一刻与用户的操作联系不起来。
		accessMode = "mapped"
	}
	frag := filesystemFragment(hostPath, tag, accessMode, boolParam(params, "read_only"))

	newXML, ierr := insertFilesystem(xmlText, frag)
	if ierr != nil {
		return failResult(ierr.Error()), nil
	}
	if derr := defineXML(ctx, newXML); derr != nil {
		return failResult("写入域定义失败: " + derr.Error()), nil
	}

	info := agent.ShareInfo{Tag: tag, Message: "共享目录已写入域定义"}
	// define 只改持久定义：运行中的域不会因此多出一个 9p 设备。如实说明，
	// 而不是让用户去来宾里找一个还不存在的挂载点。
	if state, _ := sys.DomainStateOf(ctx, vmName); state == "running" || state == "paused" {
		info.Message = "共享已写入域定义；虚拟机正在运行，9p 共享需要重启后生效"
		info.Warnings = []string{"当前运行中的虚拟机看不到这个共享，请重启后再使用"}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.ShareDataKey: info,
	}}, nil
}

// unmountShare 从域定义里移除指定 tag 的 9p 共享。
func unmountShare(ctx context.Context, vmName, tag string) (*agent.Result, error) {
	xmlText, err := sys.DumpXML(ctx, vmName, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	newXML, removed := removeFilesystem(xmlText, tag)
	if !removed {
		return failResult("该虚拟机上没有名为 " + tag + " 的共享挂载，无需卸载"), nil
	}
	if derr := defineXML(ctx, newXML); derr != nil {
		return failResult("写入域定义失败: " + derr.Error()), nil
	}

	info := agent.ShareInfo{Tag: tag, Message: "共享已从域定义移除"}
	if state, _ := sys.DomainStateOf(ctx, vmName); state == "running" || state == "paused" {
		info.Message = "共享已从域定义移除；虚拟机正在运行，需要重启后彻底消失"
		info.Warnings = []string{"当前运行中的虚拟机仍挂着这个共享，请重启后再确认"}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.ShareDataKey: info,
	}}, nil
}

// validShareAccessMode 判断 accessmode 是否在 libvirt 接受的枚举里。
func validShareAccessMode(mode string) bool {
	switch mode {
	case "mapped", "passthrough", "squash":
		return true
	}
	return false
}

// filesystemFragment 渲染 9p 共享设备片段。
//
// 属性一律转义：host_path 与 tag 来自用户输入，未转义的引号会把整段 XML
// 变成另一份文档（而 define 可能仍然成功）。
func filesystemFragment(hostPath, tag, accessMode string, readOnly bool) string {
	ro := ""
	if readOnly {
		ro = "      <readonly/>\n"
	}
	return "<filesystem type='mount' accessmode='" + xmlEsc(accessMode) + "'>\n" +
		"      <source dir='" + xmlEsc(hostPath) + "'/>\n" +
		"      <target dir='" + xmlEsc(tag) + "'/>\n" +
		ro +
		"    </filesystem>\n  "
}

// insertFilesystem 把片段插到 </devices> 之前。
func insertFilesystem(xmlText, frag string) (string, error) {
	i := strings.LastIndex(xmlText, "</devices>")
	if i < 0 {
		return "", fmt.Errorf("域定义里没有 <devices> 段，无法添加 9p 共享")
	}
	return xmlText[:i] + frag + xmlText[i:], nil
}

// shareBlock 找到包含指定 tag 的 <filesystem> 块。
type shareBlockInfo struct {
	// start / end 是块（含 </filesystem>）在 XML 里的下标。
	start, end int
	// text 是块原文，便于取 source / target。
	text string
}

// shareBlock 定位 tag 对应的 <filesystem> 段。
//
// 按块而不是按行找：`<filesystem>` 的 source/target 可能被 libvirt 折成
// 多行，逐行匹配会把「同名 tag 出现在别的设备里」也算成命中。
func shareBlock(xmlText, tag string) (shareBlockInfo, bool) {
	for i := 0; ; {
		s := strings.Index(xmlText[i:], "<filesystem")
		if s < 0 {
			return shareBlockInfo{}, false
		}
		s += i
		e := strings.Index(xmlText[s:], "</filesystem>")
		if e < 0 {
			return shareBlockInfo{}, false
		}
		e += s + len("</filesystem>")
		block := xmlText[s:e]
		if attributeOf(block, "<target", "dir") == tag {
			return shareBlockInfo{start: s, end: e, text: block}, true
		}
		i = e
	}
}

// removeFilesystem 移除 tag 对应的 <filesystem> 块（连同其缩进与行尾换行，
// 避免在定义里留下空行）。
func removeFilesystem(xmlText, tag string) (string, bool) {
	block, ok := shareBlock(xmlText, tag)
	if !ok {
		return xmlText, false
	}
	start, end := block.start, block.end
	if nl := strings.LastIndexByte(xmlText[:start], '\n'); nl >= 0 {
		// 整行都是缩进时，把缩进一起删掉。
		if strings.TrimSpace(xmlText[nl+1:start]) == "" {
			start = nl + 1
		}
	}
	if nl := strings.IndexByte(xmlText[end:], '\n'); nl >= 0 {
		end += nl + 1
	}
	return xmlText[:start] + xmlText[end:], true
}

// attributeOf 取 `<elem ... attr='v'/>` 里 attr 的值（单双引号都认）。
//
// 与 sys 包里的解析器同一层次的手段：libvirt 输出的属性顺序与引号风格
// 在这几个元素上是稳定的，不值得为一个取值引入完整 XML 解析。
func attributeOf(seg, elem, attr string) string {
	i := strings.Index(seg, elem)
	if i < 0 {
		return ""
	}
	rest := seg[i:]
	for _, q := range []string{"'", `"`} {
		key := " " + attr + "=" + q
		j := strings.Index(rest, key)
		if j < 0 {
			continue
		}
		rest = rest[j+len(key):]
		if k := strings.Index(rest, q); k >= 0 {
			return rest[:k]
		}
		return ""
	}
	return ""
}

// shareTagExists 判断域定义里是否已有该 tag 的 9p 共享。
//
// 只做字符串层面的存在性判断：这段 XML 由 libvirt 生成，`<filesystem>` 的
// 形状固定（type/mode 属性 + source dir + target dir），不值得为一个
// 存在性检查引入完整解析。
func shareTagExists(xmlText, tag string) bool {
	for _, line := range strings.Split(xmlText, "\n") {
		if !strings.Contains(line, "dir='") && !strings.Contains(line, `dir="`) {
			continue
		}
		if strings.Contains(line, "dir='"+tag+"'") || strings.Contains(line, `dir="`+tag+`"`) {
			return true
		}
	}
	return false
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
