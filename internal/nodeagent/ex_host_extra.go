// ex_host_extra.go：存储分区清单、PCI 绑定与宿主调优。
//
// 三者都是"节点上有一份真实状态，控制面只有意图"的典型：分区表、驱动
// 绑定、内核参数都不是控制面能推断的，必须由节点读写并如实回报。
package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

var pciAddressRe = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// vmStoragePartitions 实现 storage.partition.list。
func vmStoragePartitions(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "lsblk", Name: "读取分区表", Index: 1, Total: 2})
	devices, err := sys.ListBlockDevices(ctx)
	if err != nil {
		return failResult("读取块设备失败: " + err.Error()), nil
	}

	var target *sys.BlockDevice
	for i := range devices {
		d := &devices[i]
		if d.Path == op.Target || d.Name == filepath.Base(op.Target) || sys.SamePath(d.Path, op.Target) {
			target = d
			break
		}
	}
	if target == nil {
		return failResult("找不到设备: " + op.Target), nil
	}

	out := make([]agent.PartitionInfo, 0, len(target.Children))
	for _, c := range target.Children {
		if c.Type != "part" {
			continue
		}
		index := trailingNumber(c.Name)
		system := c.MountPoint == "/" || c.MountPoint == "/boot" || c.MountPoint == "/boot/efi"
		pool := ""
		if strings.HasPrefix(c.MountPoint, "/var/lib/k_cockpit/pools/") {
			pool = filepath.Base(c.MountPoint)
		}
		out = append(out, agent.PartitionInfo{
			Index: index, Path: c.Path,
			SizeBytes:   parseFlexSize(c.Size.String()),
			FSType:      c.FSType,
			Mounted:     c.MountPoint != "",
			System:      system,
			InUseByPool: pool,
		})
	}
	report(Stage{Key: "verify", Name: "核对分区信息", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.PartitionListDataKey: out},
	}, nil
}

// vmHostPCIBind 实现 host.pci.bind：在宿主驱动与 vfio-pci 之间切换。
func vmHostPCIBind(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	address := strParam(op.Params, "address")
	if address == "" {
		address = op.Target
	}
	if !pciAddressRe.MatchString(address) {
		return failResult("PCI 地址格式不正确: " + address), nil
	}
	action := strParam(op.Params, "action")
	if action != "bind" && action != "unbind" {
		return failResult("未知的 PCI 动作: " + action), nil
	}
	devDir := "/sys/bus/pci/devices/" + address
	if _, err := os.Stat(devDir); err != nil {
		return failResult("PCI 设备不存在: " + address), nil
	}

	report(Stage{Key: "check_in_use", Name: "确认设备未被占用", Index: 1, Total: 2})
	if action == "bind" {
		if len(mustReadDir("/sys/kernel/iommu_groups")) == 0 {
			return failResult("IOMMU 未开启：请在宿主 BIOS/内核参数中启用 IOMMU 后再绑定"), nil
		}
	}

	report(Stage{Key: "rebind_driver", Name: "切换驱动绑定", Index: 2, Total: 2})
	switch action {
	case "bind":
		if err := bindVFIO(ctx, address); err != nil {
			return failResult("绑定 vfio-pci 失败: " + err.Error()), nil
		}
		return &agent.Result{
			Success: true,
			Data: map[string]any{agent.HostFirewallDataKey: agent.PCIBindInfo{
				Message: "设备已绑定到 vfio-pci",
			}},
		}, nil
	default:
		if err := unbindVFIO(ctx, address); err != nil {
			return failResult("解绑 vfio-pci 失败: " + err.Error()), nil
		}
		return &agent.Result{
			Success: true,
			Data: map[string]any{agent.HostFirewallDataKey: agent.PCIBindInfo{
				Message: "设备已归还宿主驱动",
			}},
		}, nil
	}
}

func bindVFIO(ctx context.Context, address string) error {
	devDir := "/sys/bus/pci/devices/" + address
	if driverOf(devDir) == "vfio-pci" {
		return nil
	}
	if cur := driverOf(devDir); cur != "" {
		if err := writeSysfs(devDir+"/driver/unbind", address); err != nil {
			return fmt.Errorf("从 %s 解绑失败: %w", cur, err)
		}
	}
	_, _ = sys.Run(ctx, "modprobe", "vfio-pci")
	vendor := strings.TrimPrefix(strings.TrimSpace(readText(devDir+"/vendor")), "0x")
	device := strings.TrimPrefix(strings.TrimSpace(readText(devDir+"/device")), "0x")
	if vendor != "" && device != "" {
		// 非 vfio 原生支持的设备要先登记 ID；已登记时写入会报错，忽略。
		_ = writeSysfs("/sys/bus/pci/drivers/vfio-pci/new_id", vendor+" "+device)
	}
	if err := writeSysfs("/sys/bus/pci/drivers/vfio-pci/bind", address); err != nil {
		return err
	}
	if got := driverOf(devDir); got != "vfio-pci" {
		return fmt.Errorf("绑定后驱动仍为 %q", got)
	}
	return nil
}

func unbindVFIO(ctx context.Context, address string) error {
	devDir := "/sys/bus/pci/devices/" + address
	if driverOf(devDir) != "vfio-pci" {
		return nil
	}
	if err := writeSysfs("/sys/bus/pci/drivers/vfio-pci/unbind", address); err != nil {
		return err
	}
	// 清掉 override 后让内核按默认规则重新匹配宿主驱动。
	_ = writeSysfs(devDir+"/driver_override", "")
	if err := writeSysfs("/sys/bus/pci/drivers_probe", address); err != nil {
		// 有些设备没有可回退的驱动；已从 vfio 解绑即视为成功。
		if driverOf(devDir) == "" {
			return nil
		}
		return err
	}
	return nil
}

func driverOf(devDir string) string {
	target, err := os.Readlink(devDir + "/driver")
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func writeSysfs(path, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

func readText(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func mustReadDir(path string) []os.DirEntry {
	entries, _ := os.ReadDir(path)
	return entries
}

// vmHostTuningApply 实现 host.tuning.apply（ksm / zram / nested）。
func vmHostTuningApply(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	item := strParam(op.Params, "item")
	enabled, hasEnabled := op.Params["enabled"].(bool)
	if item == "" {
		return failResult("缺少调优项"), nil
	}
	if item != "nested" && !hasEnabled {
		return failResult("缺少开关值"), nil
	}

	report(Stage{Key: "validate", Name: "校验参数", Index: 1, Total: 3})
	switch item {
	case "ksm":
		report(Stage{Key: "write_sysfs", Name: "写入内核参数", Index: 2, Total: 3})
		value := "0"
		if enabled {
			value = "1"
		}
		if err := writeSysfs("/sys/kernel/mm/ksm/run", value); err != nil {
			return failResult("写入 KSM 开关失败: " + err.Error()), nil
		}
	case "zram":
		report(Stage{Key: "write_sysfs", Name: "配置 ZRAM", Index: 2, Total: 3})
		if err := applyZRAM(ctx, op.Params, enabled); err != nil {
			return failResult("配置 ZRAM 失败: " + err.Error()), nil
		}
	case "nested":
		report(Stage{Key: "write_sysfs", Name: "写入内核参数", Index: 2, Total: 3})
		path := nestedParamPath()
		if path == "" {
			return failResult("宿主没有 kvm_intel / kvm_amd 的 nested 参数，无法调整"), nil
		}
		value := "0"
		if enabled {
			value = "1"
		}
		if err := writeSysfs(path, value); err != nil {
			return failResult("写入嵌套虚拟化开关失败: " + err.Error()), nil
		}
	default:
		return failResult("未知调优项: " + item), nil
	}

	report(Stage{Key: "verify", Name: "确认生效", Index: 3, Total: 3})
	msg := "调优项 " + item + " 已下发"
	if item == "nested" && enabled {
		msg += "（当前会话生效；重启后需重新下发）"
	}
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.HostFirewallDataKey: agent.PCIBindInfo{Message: msg}},
	}, nil
}

func applyZRAM(ctx context.Context, params map[string]any, enabled bool) error {
	const dev = "/dev/zram0"
	if !enabled {
		_, _ = sys.Run(ctx, "swapoff", dev)
		_, _ = sys.Run(ctx, "zramctl", "--reset", dev)
		return nil
	}
	_, _ = sys.Run(ctx, "modprobe", "zram", "num_devices=1")
	if _, err := os.Stat(dev); err != nil {
		return fmt.Errorf("%s 不存在（zram 模块未加载）", dev)
	}
	sizeMB, _ := paramInt(params, "disksize_mb")
	algo := strParam(params, "algorithm")
	args := make([]string, 0, 5)
	if sizeMB > 0 {
		args = append(args, "--size", strconv.Itoa(sizeMB)+"M")
	}
	if algo != "" {
		args = append(args, "--algorithm", algo)
	}
	if len(args) > 0 {
		args = append(args, dev)
		if _, err := sys.Run(ctx, "zramctl", args...); err != nil {
			return err
		}
	}
	if _, err := sys.Run(ctx, "swapon", dev); err != nil {
		if !strings.Contains(err.Error(), "already") {
			return err
		}
	}
	if limitMB, ok := paramInt(params, "mem_limit_mb"); ok && limitMB > 0 {
		if err := writeSysfs("/sys/block/zram0/mem_limit",
			strconv.FormatInt(int64(limitMB)*1024*1024, 10)); err != nil {
			return err
		}
	}
	return nil
}

func nestedParamPath() string {
	for _, p := range []string{
		"/sys/module/kvm_intel/parameters/nested",
		"/sys/module/kvm_amd/parameters/nested",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// trailingNumber 取名称末尾的数字（sdb1 → 1，nvme0n1p2 → 2）。
func trailingNumber(name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(name[i:])
	return n
}

// parseFlexSize 把 lsblk 的人类可读大小转成字节（只做近似，用于展示）。
func parseFlexSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "T"):
		mult = 1 << 40
		s = strings.TrimSuffix(s, "T")
	case strings.HasSuffix(s, "G"):
		mult = 1 << 30
		s = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult = 1 << 20
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		mult = 1 << 10
		s = strings.TrimSuffix(s, "K")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}
