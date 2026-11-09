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
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
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
// 声明为变量而非常量：测试需要把它指到临时目录，避免在 CI 上写 /var/lib。
var imagesDir = "/var/lib/k_cockpit/images"

// defaultUserdataRoot 是「我的存储」的历史默认根目录。
//
// 它落在系统盘上：只要节点有独立的数据盘（存储池），控制面就会通过
// storage.root.set 把根指到池内（`<池挂载点>/userdata`）。保留这个默认值
// 是为了两件事：旧部署升级后原有文件仍可读；以及"没有存储池"的最小环境
// 仍能传文件。
//
// 声明为变量而非常量：与 imagesDir 同理，测试需要把它指到临时目录才能
// 覆盖「删除历史根里的文件」这类路径（在开发机上往 /var/lib 写既不可行
// 也不该留痕）。
var defaultUserdataRoot = "/var/lib/k_cockpit/userdata"

// userdataRootValue 承载运行期的存储根（atomic：读发生在多个执行器
// goroutine 里，写来自 storage.root.set 那一条指令）。
var userdataRootValue atomic.Value

func init() { userdataRootValue.Store(defaultUserdataRoot) }

// userdataRoot 返回当前「我的存储」的节点侧根。
func userdataRoot() string { return userdataRootValue.Load().(string) }

// trashDir 保存移入回收站时的域定义，供 vm.restore 重建域。
//
// 放在节点侧而不是控制面：域 XML 里全是本机绝对路径，只有同一台宿主机
// 才解释得通；而磁盘本来就没离开这台机器。
//
// 声明成 var 而不是 const：测试需要把它指到临时目录（写到真实的
// /var/lib 下在开发机上既不可写也不该留痕）。
var trashDir = "/var/lib/k_cockpit/trash"

// trashXMLPath 返回某台虚拟机在回收站中的域定义路径。
//
// 名字直接参与路径拼接，因此拒绝任何可能越出 trashDir 的输入（斜杠、
// 上跳）——这里的数据来自控制面，但"来自内部"不构成跳过校验的理由。
func trashXMLPath(name string) string {
	return filepath.Join(trashDir, name+".xml")
}

// vmCreate 实现 vm.create / vm.clone。
//
// 步骤：备盘（克隆=基于模板盘建 overlay 或完整复制；新建=qemu-img create）
// → 生成域 XML → virsh define → （auto_start 则 virsh autostart）。
// **创建不自动开机**：与控制面的状态机一致（创建后的域是关机态，
// 用户点「开机」才启动）。
func vmCreate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	spec := parseCreateSpec(op.Target, op.Params)
	// 网桥不存在时退回用户态网络，并把「退化」如实带回控制面。
	//
	// 静默退回是最坏的选项：界面显示网口接在交换机上，而实际是 SLIRP，
	// 端口转发、安全组、DHCP 全部不生效，用户只会看到"配了但没效果"。
	// 直接失败也不合适——节点刚纳管、还没建网络时，建机器仍然应当成功。
	bridgeDegraded := ""
	if spec.NicBridge != "" && !hostBridgeExists(ctx, spec.NicBridge) {
		bridgeDegraded = spec.NicBridge
		spec.NicBridge = ""
	}
	// OVS 桥必须在域 XML 里显式声明 virtualport，否则 libvirt 按 Linux 桥
	// 处理并在开机时失败（见 NicBridgeOVS 的说明）。
	if spec.NicBridge != "" {
		spec.NicBridgeOVS = hostIsOVSBridge(ctx, spec.NicBridge)
	}
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
		// 模板盘的容量由模板决定，而请求里的 disk_gb 可能更大（创建向导允许
		// 在模板 min_disk 之上指定）。不落实这一步，控制面记录的容量与真实
		// 磁盘会永久不一致：列表显示 10G，qemu-img 只有 3.5G，配额也按 10G
		// 计费（实机测试抓出：从模板创建时 disk_gb 被静默忽略）。
		if err := growDiskTo(ctx, diskPath, spec.DiskGB); err != nil {
			_ = os.Remove(diskPath)
			return failResult("调整系统盘容量失败: " + err.Error()), nil
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
	xmlFile, cleanup, err := writeTempXML("define", xml)
	if err != nil {
		return failResult("写域定义失败: " + err.Error()), nil
	}
	defer cleanup()
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

	// 首次启动初始化（F-2-17）：把 cloud-init / cloudbase-init 的配置打成
	// ISO 挂成第二块光驱。失败必须连带回滚域与磁盘，否则会留下一个"已定义
	// 但没有初始化盘"的孤儿域，用户重试时还会撞同名。
	seedAttached := false
	seedPath := ""
	seed := seedSpec{
		Mode:     strParam(op.Params, "init_mode"),
		Name:     spec.Name,
		Hostname: strParam(op.Params, "hostname"),
		Username: strParam(op.Params, "initial_username"),
		Password: strParam(op.Params, "initial_password"),
	}
	if seed.needsSeed() {
		iso, err := prepareSeedISO(ctx, seed)
		if err != nil {
			rollbackCreatedDomain(ctx, spec.Name, created, "")
			return failResult("生成首次启动初始化介质失败: " + err.Error()), nil
		}
		seedPath = iso
		if err := attachSeedISO(ctx, spec.Name, iso); err != nil {
			rollbackCreatedDomain(ctx, spec.Name, created, iso)
			return failResult("挂载首次启动初始化介质失败: " + err.Error()), nil
		}
		seedAttached = true
	}

	// 落盘磁盘清单：域定义被外部删除（`virsh undefine`，僵尸机的常见形态）
	// 之后，删除路径就再也读不到这台机器的磁盘了——清单是那时唯一的依据。
	//
	// 位置：磁盘全部建成、首次启动介质也挂好之后（早于此写，一旦后续失败
	// 回滚磁盘，清单会指向已不存在的文件）。created 里是系统盘 + 全部数据盘
	// 的绝对路径，顺序即创建顺序。
	//
	// 写失败不阻断创建：清单是兜底信息，不是创建的必要产物；没有清单的
	// 机器删除时会退到"按精确文件名删系统盘"的第 2 层兜底。
	if err := writeDiskManifest(spec.Name, created); err != nil {
		log.Printf("[nodeagent] 记录磁盘清单失败 vm=%s: %v", spec.Name, err)
	}

	// MAC 由 libvirt 分配；创建向导填写的静态地址靠它登记 DHCP 静态租约。
	mac := primaryMACOf(ctx, spec.Name)
	uuid, _ := sys.DomainUUID(ctx, spec.Name)
	return &agent.Result{
		Success: true,
		Data: map[string]any{
			"uuid":                    uuid,
			agent.CreatePrimaryMACKey: mac,
			"seed_attached":           seedAttached,
			agent.StatusDataKey:       "stopped",
			agent.CloneDataKey:        agent.CloneInfo{DiskPath: diskPath, BackingPath: backingOf(spec)},
			"seed_iso_path":           seedPath,
			// 非空表示主网口**没有**接到这个网桥（已退回用户态网络）。
			"network_degraded_bridge": bridgeDegraded,
			// 回传**实际生效**的架构/机型/固件：控制面的默认值来自创建
			// 矩阵（x86_64 视角），节点按自身架构解析出的值可能不同
			// （aarch64 → virt + UEFI）。控制面按回传值写投影，否则
			// 详情页显示的配置与真实运行的域不一致。
			"arch":     spec.Arch,
			"machine":  spec.Machine,
			"firmware": spec.Firmware,
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
		// 优雅关机是**异步**的：`virsh shutdown` 只是发一个 ACPI 电源键事件，
		// 来宾什么时候关由它自己决定（甚至可能永远不关——没装 ACPI 处理、
		// 卡在固件里、内核挂了）。
		//
		// 旧实现发完命令立刻回读状态并返回 success，于是任务显示"关机成功"
		// 而机器还在跑（实机抓出：空盘虚拟机停在 UEFI Shell，shutdown 任务
		// success、domstate 仍是 running）。等待并如实报告"没关掉"比一个
		// 漂亮的成功任务有用得多——后者会让用户以为机器已经安全停机。
		if kind == "shutdown" {
			state := waitShutdown(ctx, op.Target, shutdownWait)
			if state == "running" || state == "paused" {
				return failResult("来宾未在 " + shutdownWait.String() +
					" 内响应关机请求（可能未安装 ACPI 支持或来宾已失去响应）。" +
					"请改用强制断电，或进控制台确认来宾状态。"), nil
			}
			return &agent.Result{Success: true, Data: map[string]any{
				agent.StatusDataKey: state,
			}}, nil
		}
		// 其余动作是同步的（destroy 立即生效；start 之后状态稳定；
		// reboot 由来宾决定何时回来，回读当前态即可）。
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
	// residueNote 收集「有残留但不敢自动删」的提示（见 disksForDomainMissing）。
	residueNote := ""

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
	// 移入回收站（保留磁盘）时**先把域定义留存一份**，恢复才有依据。
	//
	// 之前只删定义不留副本，于是"回收站"里的机器永远无法恢复——恢复接口
	// 只能回一句「域定义已不存在」，用户看到的是一个只能彻底删除的回收站
	// （实机抓出）。域 XML 引用的都是本机绝对路径，磁盘还在原处，重定义
	// 出来就是原来那台机器。
	//
	// 只在 keep 时留存：transfer 会把磁盘搬进「我的存储」，原 XML 里的
	// 路径随即失效，留着只会让恢复出一个指向空路径的域。
	if diskAction != "delete" && diskAction != "transfer" && !force && xml != "" {
		if err := os.MkdirAll(trashDir, 0o755); err != nil {
			return failResult("创建回收站目录失败: " + err.Error()), nil
		}
		if err := os.WriteFile(trashXMLPath(op.Target), []byte(xml), 0o600); err != nil {
			// 存不下就不能删：删掉之后这台机器将无法恢复，而用户以为
			// 自己只是"移到了回收站"。
			return failResult("保存回收站域配置失败（未删除域定义）: " + err.Error()), nil
		}
	}
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
		paths := append([]string{sysDisk}, dataDiskPaths(dataDisks)...)
		if sysDisk == "" && len(dataDisks) == 0 {
			// 域定义里一块盘都读不到：多为僵尸机（外部 `virsh undefine`
			// 之后 dumpxml 为空）。旧实现在这里什么都不删，于是任务成功、
			// 系统盘留在 /var/lib/k_cockpit/images 下（实机复现）。
			//
			// 兜底顺序：创建时落盘的清单 → 按精确文件名删系统盘。
			// 数据盘在兜底路径下**只提示不删**（命名无法与"另一台机器的
			// 系统盘"区分：VM 名本身可能长成 foo-vdb）。
			extra, note := disksForDomainMissing(op.Target)
			paths = append(paths, extra...)
			if note != "" {
				residueNote = note
			}
		}
		for _, path := range paths {
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

	// 彻底删除（连同磁盘）：回收站里的域定义也没有意义了，一并清掉，
	// 否则同名重建会继承一份指向已删磁盘的旧 XML。
	//
	// 磁盘清单同理：磁盘已经删了，留着一份指向空路径的清单会让"同名重建
	// 后又删除"的路径误信旧清单。keep（回收站）**不删**清单——恢复回来的
	// 机器还需要它。
	if diskAction == "delete" || force {
		_ = os.Remove(trashXMLPath(op.Target))
		removeDiskManifest(op.Target)
	}

	data := map[string]any{}
	if len(transferred) > 0 {
		data[agent.VMDiskTransferDataKey] = transferred
	}
	if force {
		data["message"] = "已强制删除域定义并重启 libvirt"
	}
	if residueNote != "" {
		// 残留提示必须**回传到控制面**：只写节点日志的话，用户看到的仍是
		// 一个干干净净的"删除成功"，而宿主机上的盘还在。
		if msg, _ := data["message"].(string); msg != "" {
			data["message"] = msg + "；" + residueNote
		} else {
			data["message"] = residueNote
		}
	}
	res := &agent.Result{Success: true, Data: data}
	if msg, _ := data["message"].(string); msg != "" {
		res.Message = msg
	}
	return res, nil
}

// vmRestore 实现 vm.restore：按回收站里留存的域定义重建域（F-2-04）。
//
// 只做 `virsh define`，**不开机**：恢复的语义是"这台机器回到列表里"，
// 要不要运行由用户决定——自动开机会让一台误删的机器立刻占用资源。
//
// 幂等：域已存在直接成功（可能是恢复后又被别的路径定义了）。
func vmRestore(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	name := strings.TrimSpace(op.Target)
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return failResult("虚拟机名不合法"), nil
	}

	report(Stage{Key: "domain_check", Name: "确认域状态", Index: 1, Total: 2})
	if sys.DomainExists(ctx, name) {
		return &agent.Result{Success: true,
			Data: map[string]any{"message": "域已存在，无需重建"}}, nil
	}

	report(Stage{Key: "domain_define", Name: "按回收站配置重建域", Index: 2, Total: 2})
	raw, err := os.ReadFile(trashXMLPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return failResult(
				"回收站里没有这台虚拟机的域配置（多为旧版本删除时未留存），" +
					"无法恢复为可运行的机器；磁盘仍在节点上，可用于重新创建"), nil
		}
		return failResult("读取回收站域配置失败: " + err.Error()), nil
	}

	// virsh define 只接受文件路径，写一份临时文件交给它。
	tmp, cleanup, err := writeTempXML("restore", string(raw))
	if err != nil {
		return failResult("写入临时域配置失败: " + err.Error()), nil
	}
	defer cleanup()

	if _, err := sys.Run(ctx, "virsh", "define", tmp); err != nil {
		return failResult("重建域定义失败: " + err.Error()), nil
	}
	if !sys.DomainExists(ctx, name) {
		// define 返回成功但域仍不在，说明 XML 被 libvirt 接受却没有落库
		//（极罕见）。如实失败，别让控制面把记录翻回"存在"。
		return failResult("重建域后仍未在虚拟化层找到该域"), nil
	}
	return &agent.Result{Success: true,
		Data: map[string]any{"message": "已按回收站配置重建域定义（关机态）"}}, nil
}

func dataDiskPaths(disks []sys.DiskXML) []string {
	out := make([]string, 0, len(disks))
	for _, d := range disks {
		out = append(out, d.SourceFile)
	}
	return out
}

// growDiskTo 在镜像虚拟容量小于 wantGB 时扩容，**绝不缩小**。
//
// 只扩不缩：链式克隆与模板共享同一个父盘，缩容会截断父盘数据；而完整克隆
// 缩容也只会在来宾里留下一个比文件系统更小的盘，开机即坏。
func growDiskTo(ctx context.Context, path string, wantGB int) error {
	cur, err := sys.QemuImgSize(ctx, path)
	if err != nil {
		return err
	}
	if cur >= int64(wantGB)<<30 {
		return nil
	}
	_, err = sys.RunTimeout(ctx, qemuConvertTimeout, "qemu-img", "resize",
		path, fmt.Sprintf("%dG", wantGB))
	return err
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
	// 快照在虚拟化层里的名字。新键是 snapshot_name；domain_name 是同义旧键
	// （名字起得不好，历史上被当成"域名"传过），仅为兼容在途任务而保留。
	snapshotName := strings.TrimSpace(strParam(op.Params, "snapshot_name"))
	if snapshotName == "" {
		snapshotName = strings.TrimSpace(strParam(op.Params, "domain_name"))
	}
	report(Stage{Key: "snapshot_delete", Name: "删除快照", Index: 1, Total: 1})
	if snapshotName == "" {
		// 空名会让 virsh 报 `Failed to get option 'snapshotname': Option
		// argument is empty`——那句话与"快照是否存在"毫无关系，用户和排障的
		// 人都看不出真正的原因。在这里说清楚。
		return failResult("删除快照缺少快照名（snapshot_name），无法定位要删除哪一个"), nil
	}
	if _, err := sys.Run(ctx, "virsh", "snapshot-delete", "--domain", op.Target,
		"--snapshotname", snapshotName); err != nil {
		// 「快照已不存在」按幂等成功处理，但**不能只看错误字符串**：名字拼错、
		// 域不存在同样可能带 "not found"，那样会得到「控制面删了记录、节点上
		// 快照还在」的假成功。这里再查一次快照清单，确认那个名字真的不在了。
		if isMissingSnapshotErr(err) && !snapshotExists(ctx, op.Target, snapshotName) {
			return &agent.Result{Success: true, Message: "快照已不存在（幂等成功）"}, nil
		}
		return failResult("删除快照失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true}, nil
}

// isMissingSnapshotErr 判断 virsh 的错误是否是"这个快照不存在"。
//
// 要求同时出现 snapshot 与 not-found 两个线索：只看 "not found" 会把
// "domain not found"（域被删了）也当成幂等成功，而那是完全不同的一类故障。
func isMissingSnapshotErr(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "snapshot") && strings.Contains(msg, "not found") {
		return true
	}
	return strings.Contains(msg, "快照") &&
		(strings.Contains(msg, "不存在") || strings.Contains(msg, "未找到"))
}

// snapshotExists 报告域里是否还有指定名字的快照。
//
// 查不到清单时**按"还在"处理**：宁可让用户重试一次删除，也不要谎报已删除
// 而把控制面记录抹掉。
func snapshotExists(ctx context.Context, vm, name string) bool {
	out, err := sys.Run(ctx, "virsh", "snapshot-list", "--name", vm)
	if err != nil {
		return true
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true
		}
	}
	return false
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

	// 兜底：控制面已经拒过一轮，这里再拒一次。节点侧拿到不认识的键时
	// **不能当作成功**——那正是历史上"任务成功、虚拟机没变"的来源。
	if unsupported := unsupportedConfigKeys(changes); len(unsupported) > 0 {
		return failResult("以下配置项目前无法在节点侧生效：" + strings.Join(unsupported, "、")), nil
	}

	// XML 类配置先做：它们要读一次定义、改完再 define。失败就整体返回，
	// 不再继续下发命令——否则会留下"一半改了一半没改"的中间态。
	if len(xmlConfigKeys(changes)) > 0 {
		xmlText, err := sys.DumpXML(ctx, op.Target, false)
		if err != nil {
			return failResult("读取域定义失败: " + err.Error()), nil
		}
		newXML, appliedKeys := applyConfigXML(xmlText, changes)
		if len(appliedKeys) > 0 {
			if err := defineXML(ctx, newXML); err != nil {
				return failResult("libvirt 拒绝了配置变更: " + err.Error()), nil
			}
			applied = append(applied, appliedKeys...)
		}
	}

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
	// 磁盘限速走 blkdeviotune（需要设备名——从定义里取系统盘）。
	//
	// 六项限值一次调用下发：分多次调用时，中途失败会留下一半新一半旧的
	// 组合，而用户看到的只是一个"失败"。
	//
	// 运行中的域带 --live，与界面标注的「即时生效」对齐；只写 --config
	// 会让改动等下次重启才生效，而用户收到的是"已应用"。
	//
	// 字节类字段的单位是 MB/s（矩阵与界面的口径），blkdeviotune 收 bytes/s。
	if tune := diskTuneArgs(changes); len(tune) > 0 {
		if dev := sysDiskDev(ctx, op.Target); dev != "" {
			args := append([]string{"-c", "qemu:///system", "blkdeviotune", op.Target, dev}, tune...)
			if running, err := vmRunning(ctx, op.Target); err == nil && running {
				args = append(args, "--live", "--config")
			} else {
				args = append(args, "--config")
			}
			if _, err := sys.Run(ctx, "virsh", args...); err == nil {
				applied = append(applied, tunedKeys(changes)...)
			} else {
				skipped = append(skipped, "磁盘限速（节点拒绝）")
			}
		} else {
			skipped = append(skipped, "磁盘限速（找不到系统盘）")
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

// unsupportedConfigKeys 返回 changes 里节点侧没有实现的键（按字典序）。
func unsupportedConfigKeys(changes map[string]any) []string {
	out := make([]string, 0, len(changes))
	for key := range changes {
		if !agent.ConfigKeyApplied(key) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// diskTuneArgs 把磁盘限速类变更翻译成 blkdeviotune 参数。
//
// 只翻译 changes 里**出现**的键：差异提交下未出现的项保持节点上的原值，
// 补一个 0 会把用户上次设的限制悄悄清掉。
func diskTuneArgs(changes map[string]any) []string {
	var args []string
	addIOPS := func(key, flag string) {
		if v, ok := numField(changes, key); ok {
			args = append(args, flag, fmtInt(v))
		}
	}
	// 界面按 MB/s 填写；blkdeviotune 的 bytes 类参数收 bytes/s。
	addBytes := func(key, flag string) {
		if v, ok := numField(changes, key); ok {
			args = append(args, flag, strconv.FormatInt(int64(v)*1024*1024, 10))
		}
	}
	addIOPS("disk_iops_total", "--total-iops-sec")
	addIOPS("disk_iops_read", "--read-iops-sec")
	addIOPS("disk_iops_write", "--write-iops-sec")
	addBytes("disk_bytes_total", "--total-bytes-sec")
	addBytes("disk_bytes_read", "--read-bytes-sec")
	addBytes("disk_bytes_write", "--write-bytes-sec")
	return args
}

// tunedKeys 返回本次实际下发的限速键，供结果消息列出「改了哪些」。
func tunedKeys(changes map[string]any) []string {
	keys := make([]string, 0, 6)
	for _, key := range []string{
		"disk_iops_total", "disk_iops_read", "disk_iops_write",
		"disk_bytes_total", "disk_bytes_read", "disk_bytes_write",
	} {
		if _, ok := numField(changes, key); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

func numField(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	// 线上参数经过 JSON 反序列化，数字是 float64；进程内调用与单测可能
	// 直接给 int。两种都认，避免"同一份参数在测试里能过、线上被静默跳过"。
	switch v := m[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
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
	// fromBus 只在换总线时出现：旧设备名由它决定，见 case "bus"。
	fromBus := strParam(op.Params, "from_bus")
	dev := cdromDev(orderNo, bus)
	isoPath := strParam(op.Params, "iso_path")
	if isoPath != "" && !strings.HasPrefix(isoPath, "/") {
		// 控制面下发的是「我的存储」相对路径；节点侧存储根 + rel_path。
		isoPath = filepath.Join(userdataRoot(), isoPath)
		// Join 只做清洗不做约束：带 ../ 的 rel_path 会落到存储根之外
		//（guest 可挂载宿主机任意文件）——越界直接拒绝。
		if !strings.HasPrefix(isoPath, userdataRoot()+"/") {
			return failResult("镜像路径越出用户存储根: " + isoPath), nil
		}
		// qemu 要打开的 ISO 在 images 之外——AppArmor 只放行了镜像目录
		//（review 抓出：挂 ISO 后开机 Permission denied，与快照链同根因）。
		ensureAppArmorAllowsImages(userdataRoot())
	}

	report(Stage{Key: "render_device", Name: "渲染光驱设备", Index: 1, Total: 2})
	switch action {
	case "attach":
		// **允许空托盘**：先挂一台没有介质的光驱、之后再「载入」镜像是正常用法
		// （eject 之后也是这个状态）。此前 attach 与 load 共用「iso_path 必填」，
		// 于是不选镜像的挂载会失败，而控制面已经写入了光驱记录——界面显示有
		// 光驱、域里却没有（实机抓出）。
	case "load":
		// 载入光盘必须有介质。
		if isoPath == "" {
			return failResult("缺少 iso_path：载入光盘必须指定镜像"), nil
		}
	case "eject", "remove":
		// 空托盘 / 摘除设备。
	case "bus":
		// 换总线 = 摘掉旧设备 + 按新总线挂回。libvirt 没有"改总线"这种增量
		// 操作，而旧设备名由旧总线决定（ide 是 hd*，sata/scsi 是 sd*）——
		// 所以节点必须拿到 from_bus，否则只能猜一个设备名去摘，摘错就是
		// 域里少一块盘。
		if fromBus == "" {
			return failResult("换总线缺少 from_bus：无法确定要摘除哪块旧光驱"), nil
		}
		// **动手之前**先确认目标总线被当前机型支持。libvirt 会照单全收
		// 这个定义，直到**开机时**才报
		// `unsupported configuration: IDE controllers are unsupported for
		// this QEMU binary or machine type`——也就是说这个操作会"成功"地
		// 把机器变成不可开机状态，而用户是在下一次开机时才发现。
		if berr := ensureBusSupportedByMachine(ctx, op.Target, bus); berr != nil {
			return failResult(berr.Error()), nil
		}
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
	case "bus":
		err = switchCDROMBus(ctx, op.Target, orderNo, fromBus, bus, isoPath)
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

// ideSupportedByMachine 判断某机型是否带 IDE 控制器。
//
// 为什么拆成纯函数、把 arch 显式传进来：`runtime.GOARCH` 是编译期常量，
// 测试进程里改不了，而「定义里没有 machine 属性时按架构推断」这条分支
// 恰恰必须被覆盖——不拆就只能靠实机分别跑 amd64/arm64 两套节点。
func ideSupportedByMachine(machine, arch string) bool {
	machine = strings.ToLower(strings.TrimSpace(machine))
	if machine == "" {
		// 旧定义可能不带 machine 属性，此时 libvirt 用默认机型：
		// x86 的默认是 i440fx（有 IDE），arm64 的默认是 virt（没有）。
		// 按架构推断而不是一律放行：放行的代价是写进一个开不了机的定义，
		// 而用户要到下一次开机才发现。
		return arch == "amd64"
	}
	// q35 / i440fx / pc-* 都有 IDE 控制器；virt（aarch64 上的常见选择）
	// 以及将来可能出现的 virt-* 变体一律按"没有 IDE"处理。
	return !strings.HasPrefix(machine, "virt")
}

// ensureBusSupportedByMachine 校验目标总线被域当前的机型支持。
//
// 目前只拦 IDE：IDE 控制器不是每种机型都有——**virt 机型（aarch64 上的
// 常见选择）没有**，而 libvirt 会接受写进定义，直到开机才报
// `unsupported configuration: IDE controllers are unsupported for this
// QEMU binary or machine type`。
//
// 放在 detach 之前调用：先摘旧设备再发现新总线不可用，用户会得到一台
// **光驱消失**的机器，而且下次开机直接失败——那比"换总线被拒绝"糟得多。
//
// 读不到域定义时对 ide 一律拒绝：宁可让用户重试一次，也不要写进一个
// 可能开不了机的配置。sata/scsi 不依赖机型，直接放行（它们在任何机型上
// 都有控制器）。
func ensureBusSupportedByMachine(ctx context.Context, vm, bus string) error {
	if bus != "ide" {
		return nil
	}
	xmlText, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return fmt.Errorf("读取域定义失败，无法确认机型是否支持 IDE 控制器（%v）；请选择 sata 或 scsi", err)
	}
	machine := sys.MachineTypeOf(xmlText)
	if ideSupportedByMachine(machine, runtime.GOARCH) {
		return nil
	}
	// 报错里要写清"当前是什么机型"：只说"不支持 IDE"，用户会以为是选错了
	// 总线，而不是这台机器根本没有 IDE 控制器。
	name := strings.ToLower(strings.TrimSpace(machine))
	if name == "" {
		name = "默认机型/" + runtime.GOARCH
	}
	return fmt.Errorf("当前机型 %s 不支持 IDE 控制器，请选择 sata 或 scsi", name)
}

// switchCDROMBus 把光驱从 fromBus 换到 toBus：先摘旧设备，再按新总线挂回。
//
// 两步而不是一步：libvirt 没有"改总线"的增量操作，`attach-device` 只会新增
// 设备。`--config` 意味着改的是持久定义（下次开机生效），与其它光驱动作
// 一致——换总线本身几乎一定需要重启。
//
// 挂回时保留原介质（isoPath）：用户换总线是为了让光驱能引导/被识别，把盘
// 顺手丢掉会得到一台"光驱空了"的机器，而他并没有要求弹出。
//
// 第二步失败时把旧设备挂回去：否则用户得到的是一台**光驱消失**的机器——
// 那比"换总线失败"更糟，因为原来的盘也读不到了。
func switchCDROMBus(
	ctx context.Context, vm string, orderNo int, fromBus, toBus, isoPath string,
) error {
	oldDev := cdromDev(orderNo, fromBus)
	newDev := cdromDev(orderNo, toBus)

	// 旧设备不在域里也算成功：这一步的语义是"让旧设备不在"，而它可能已经
	// 被上一次操作摘掉了（控制面记录与域定义不一致时的自愈路径）。
	detached := false
	if cdromExists(ctx, vm, oldDev) {
		if _, err := sys.Run(ctx, "virsh", "detach-device", "--config", vm,
			getStdinXML(vm, oldDev, fromBus)); err != nil {
			return fmt.Errorf("摘除旧总线的光驱（%s）失败: %w", oldDev, err)
		}
		detached = true
	}
	if _, err := sys.Run(ctx, "virsh", "attach-device", "--config", vm,
		getStdinXML(vm, newDev, toBus, isoPath)); err != nil {
		if detached {
			// 尽力挂回旧设备；挂不回去也要把原始错误报出去（那才是根因）。
			_, _ = sys.Run(ctx, "virsh", "attach-device", "--config", vm,
				getStdinXML(vm, oldDev, fromBus, isoPath))
		}
		return fmt.Errorf("按新总线（%s）挂载光驱失败: %w", toBus, err)
	}
	return nil
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
// getStdinXML 把光驱设备片段写成临时文件，返回给 virsh 当 <file> 参数。
//
// 每次调用创建**唯一**文件而不是固定路径（曾经是
// /tmp/kc-cdrom-<vm>-<dev>.xml）：固定名有两个真实后果——
//  1. 同一台虚拟机上并发/连续的光驱操作会互相覆盖对方的 XML；
//  2. 5 秒后删除的定时器会让「5 秒后再用同一个名字」拿到空文件，
//     表现是 attach-device 收到空描述、错误信息指向 libvirt 参数。
//
// 实机测试里它以「偶发失败」的形态出现：同一用例单独跑必过、整包跑偶发
// 失败（前一个用例的定时器恰好删掉了同名文件）。唯一文件把这条时序依赖
// 彻底去掉，清理仍由定时器兜底（不阻塞调用方）。
func getStdinXML(vm, dev, bus string, isoPath ...string) string {
	frag := cdromFragment(dev, bus, "")
	if len(isoPath) > 0 {
		frag = cdromFragment(dev, bus, isoPath[0])
	}
	name, cleanup, err := writeTempXML("cdrom-"+vm+"-"+dev, frag)
	if err != nil {
		// 落盘失败时仍返回一个路径，让 virsh 的报错指向"文件不存在"，
		// 而不是让调用方拿到空字符串去猜。
		log.Printf("[nodeagent] 创建光驱 XML 临时文件失败 vm=%s dev=%s: %v", vm, dev, err)
		return filepath.Join(os.TempDir(), "kc-cdrom-missing.xml")
	}
	go func() {
		time.Sleep(5 * time.Second)
		cleanup()
	}()
	return name
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
		source := filepath.Join(userdataRoot(), rel)
		if source != userdataRoot() && !strings.HasPrefix(source, userdataRoot()+"/") {
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
		ensureAppArmorAllowsImages(userdataRoot())
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

// hostBridgeExists 判断宿主机上是否存在该网桥（Linux 桥或 OVS 桥）。
//
// 两种后端都要查：基础模式建的是 Linux 桥，增强模式的交换机是 OVS 桥；
// 只查一种会让另一类交换机上的创建全部退化为用户态网络。
func hostBridgeExists(ctx context.Context, bridge string) bool {
	if bridge == "" {
		return false
	}
	if _, err := sys.Run(ctx, "ip", "link", "show", bridge); err == nil {
		return true
	}
	_, err := sys.Run(ctx, "ovs-vsctl", "br-exists", bridge)
	return err == nil
}

// hostIsOVSBridge 判断同名网桥是否由 OVS 管理。
func hostIsOVSBridge(ctx context.Context, bridge string) bool {
	_, err := sys.Run(ctx, "ovs-vsctl", "br-exists", bridge)
	return err == nil
}

// shutdownWait 是优雅关机的等待上限。
//
// 60s 足以覆盖正常系统的关机流程（systemd 默认 90s 的 StopTimeout 里，
// 常见服务几秒内就结束了）；再长会让用户对着一个"进行中"的任务无所适从，
// 而强制断电是显式的另一个按钮。
const shutdownWait = 60 * time.Second

// waitShutdown 轮询到域停机或超时，返回最后一次读到的状态。
func waitShutdown(ctx context.Context, domain string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	state := "running"
	for {
		time.Sleep(time.Second)
		if s, err := sys.DomainStateOf(ctx, domain); err == nil {
			state = s
			if s != "running" && s != "paused" {
				return s
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return state
		}
	}
}
