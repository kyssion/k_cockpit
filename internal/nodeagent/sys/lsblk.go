// lsblk.go：块设备探测（node.disks 与分区清单的来源）。
package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BlockDevice 是 lsblk -J 的一台设备（含分区子树）。
//
// Size 是 FlexSize：不带 -b 时是 "128G" 字符串、带 -b 时是数字字节，
// 两种发行版形态都要接住。
type BlockDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Size       FlexSize      `json:"size"`
	Type       string        `json:"type"`
	FSType     string        `json:"fstype"`
	MountPoint string        `json:"mountpoint"`
	Children   []BlockDevice `json:"children"`
}

// FlexSize 接住 lsblk 的两种容量形态（人类可读字符串 / 字节数字）。
type FlexSize string

// UnmarshalJSON 实现：数字与字符串都收，其余原样存文本（解析交给
// parseSizeBytes，它对无效值返回 0 而不是让整次探测失败）。
func (f *FlexSize) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	*f = FlexSize(s)
	return nil
}

// String 取原始文本。
func (f FlexSize) String() string { return string(f) }

type lsblkOutput struct {
	BlockDevices []BlockDevice `json:"blockdevices"`
}

// ListBlockDevices 枚举整盘（type=disk），解析挂载与数据占用状态。
//
// 判定口径（与 agent.Disk 的字段一一对应）：
//   - IsSystem：设备或其任一子分区承载 /（根文件系统）——用 findmnt 核实，
//     不猜设备名（sda/vda 在另一台机器上可能就是数据盘）；
//   - Mounted：任一分区的 mountpoint 非 null（含根分区之外的）；
//   - HasData：存在分区子树，或设备自身有可识别文件系统。
func ListBlockDevices(ctx context.Context) ([]BlockDevice, error) {
	out, err := Run(ctx, "lsblk", "-J", "-b",
		"-o", "NAME,PATH,SIZE,TYPE,FSTYPE,MOUNTPOINT")
	if err != nil {
		return nil, err
	}
	var parsed lsblkOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return nil, fmt.Errorf("lsblk 输出解析失败: %w", err)
	}
	return parsed.BlockDevices, nil
}

// RootDevicePath 返回承载根文件系统的整盘路径（如 /dev/vda）；找不到返回空。
func RootDevicePath(ctx context.Context) string {
	// findmnt -n -o SOURCE / 给出源设备（可能是分区或子卷路径）。
	out, err := Run(ctx, "findmnt", "-n", "-o", "SOURCE", "/")
	if err != nil {
		return ""
	}
	src := strings.TrimSpace(out)
	if src == "" {
		return ""
	}
	// btrfs 子卷会带 [/subvol] 后缀，先剥掉。
	if i := strings.IndexByte(src, '['); i > 0 {
		src = src[:i]
	}
	// 分区名 → 整盘名：按 sysfs 的 parents 链向上找一层。
	out2, err := Run(ctx, "lsblk", "-npr", "-o", "PKNAME", src)
	if err != nil {
		return ""
	}
	pk := strings.TrimSpace(out2)
	if pk == "" {
		// 源本身就是整盘（无分区表的根盘）。
		if strings.HasPrefix(src, "/dev/") {
			return src
		}
		return ""
	}
	return "/dev/" + pk
}

// StableDeviceID 返回设备的稳定标识（/dev/disk/by-id 下的第一个非 usb 条目）。
//
// by-id 目录里的条目包含 usb-*（U 盘、读卡器），它的稳定性比 nvme-/ata-
// 差得多（拔插重插就变），因此排序后优先取非 usb 前缀。
func StableDeviceID(devPath string) string {
	entries, err := filepath.Glob("/dev/disk/by-id/*")
	if err != nil || len(entries) == 0 {
		return devPath // 找不到 by-id（极端精简环境）时退回路径本身
	}
	want, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		return devPath
	}
	var fallback string
	for _, e := range entries {
		resolved, err := filepath.EvalSymlinks(e)
		if err != nil || resolved != want {
			continue
		}
		base := filepath.Base(e)
		if !strings.HasPrefix(base, "usb-") {
			return e
		}
		fallback = e
	}
	if fallback != "" {
		return fallback
	}
	return devPath
}

// HasPartitionsOrFS 报告设备是否有分区子树或自身文件系统（HasData 口径）。
func HasPartitionsOrFS(d BlockDevice) bool {
	if len(d.Children) > 0 {
		return true
	}
	return d.FSType != "" && d.FSType != "iso9660"
}

// SubtreeMounted 报告设备或其任一子分区处于挂载状态。
func SubtreeMounted(d BlockDevice) bool {
	if d.MountPoint != "" {
		return true
	}
	for _, c := range d.Children {
		if SubtreeMounted(c) {
			return true
		}
	}
	return false
}

// SubtreeRootFS 报告设备子树里是否挂着根文件系统（IsSystem 口径之一）。
func SubtreeRootFS(d BlockDevice, rootSrc string) bool {
	for _, c := range d.Children {
		if rootSrc != "" && (c.Path == rootSrc || SamePath(c.Path, rootSrc)) {
			return true
		}
		if SubtreeRootFS(c, rootSrc) {
			return true
		}
	}
	return false
}

// SamePath 用 inode 比较两个设备路径是否同一块盘（处理 by-id 与
// /dev/vda 指向同一设备的情况）。
func SamePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	var sa, sb os.FileInfo
	sa, err1 = os.Stat(ra)
	sb, err2 = os.Stat(rb)
	if err1 != nil || err2 != nil {
		return false
	}
	return os.SameFile(sa, sb)
}
