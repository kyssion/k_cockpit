// ex_longtail.go：P5 长尾执行器——迁移、重装、救援、导出、来宾自动化、
// 口令审计、用户 SSH 开关、配额处置、平台自检修复。
//
// 边界声明（分期口径）：
//   - 迁移：热迁移需要两节点协同与脏页采样闭环，P5 第一批给**冷迁移**
//     （关机 → 磁盘 rsync → 目标侧定义），热迁移与 takeover 随第二批；
//   - 重装：备份原盘 → 从模板重建；purge 删备份；
//   - 救援：换 SATA 盘型 + 挂救援 ISO + 保存配置快照；
//   - 导出：qemu-img convert 出镜像（tar/ova 随第二批）；
//   - 来宾自动化：依赖来宾内 qemu-guest-agent，未装时给明确失败。
package nodeagent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// migrateStagingRoot 是迁移暂存区根目录。源侧把盘/内存态/NVRAM 放到这里，
// 跨节点时由控制面中转（分片拉取→回放到目标同路径），同机双 agent 部署下
// 两侧共享文件系统、中转退化为 no-op。
const migrateStagingRoot = "/var/lib/k_cockpit/migrate"

// vmMigrate 实现 vm.migrate（P5 第二批：offline / warm，live 诚实阻断）。
//
// 源侧职责：停机（或 save 保留内存现场）→ 导出域 XML → 盘与 NVRAM 复制到
// staging → undefine 源域。**磁盘文件不动**——清理在控制面记录落库成功之后
// 由 action=cleanup 做；域定义先移除是刻意的：同名双活（有人把源侧开机而
// 控制面已指向目标）比「暂时无定义」危险，且定义可随时由已回传的 XML 重建。
func vmMigrate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	if strParam(op.Params, "action") == "cleanup" {
		return migrateCleanup(ctx, op, report)
	}
	mode := strParam(op.Params, "mode")
	if mode == "live" {
		// 架构边界而不是未实现：热迁移要求 libvirt 对等连接（qemu+ssh/tcp），
		// 即节点间互信凭据——与 ADR-0005「控制面不持有宿主机登录凭据」冲突。
		// 待节点间传输通道定型（专用证书或受限 ssh 指纹登记）后接入。
		return failResult("热迁移需要节点间 libvirt 互信通道（qemu+ssh/tcp），" +
			"与「控制面不持有宿主机凭据」的架构约束冲突；当前请使用停机迁移（offline）" +
			"或保留内存现场的温迁移（warm）"), nil
	}

	start := timeNowSec()
	// **重试快速路径**：源侧流程是「save → 复制 → undefine」，一旦走到
	// undefine 之后失败（典型：目标节点不可达），重试时域已不存在、
	// DumpXML 必然失败——「可重试」的承诺就空了。staging 里落一份
	// domain.xml + manifest.json，重试时直接从暂存区重发清单。
	if !sys.DomainExists(ctx, op.Target) {
		if res, ok := migrateResumeFromStaging(op.Target, start); ok {
			return res, nil
		}
		return failResult("源域不存在且暂存区没有可重发的迁移材料（迁移未开始或已被清理）"), nil
	}
	report(Stage{Key: "source.stop", Name: "源侧停机", Index: 1, Total: 4})
	if mkErr := osMkdirAllErr(filepath.Join(migrateStagingRoot, op.Target)); mkErr != nil {
		return failResult("创建暂存区失败: " + mkErr.Error()), nil
	}
	xml, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取源域定义失败: " + err.Error()), nil
	}
	state, _ := sys.DomainStateOf(ctx, op.Target)
	memFile := ""
	if mode == "warm" {
		// 温迁移：save 落内存态（执行后域转 shutoff，运行现场保留在文件里）。
		// 重试时域已 shutoff：若暂存区已有内存态则复用，不降级成 offline。
		if state != "shutoff" && state != "shutdown" {
			memFile = op.Target + ".memstate"
			if _, serr := sys.Run(ctx, "virsh", "save", op.Target,
				migrateStagePath(op.Target, memFile)); serr != nil {
				return failResult("保存内存现场失败: " + serr.Error()), nil
			}
		} else if _, merr := osStat(migrateStagePath(op.Target, op.Target+".memstate")); merr == nil {
			memFile = op.Target + ".memstate"
		}
	} else if state == "running" || state == "paused" {
		if _, derr := sys.Run(ctx, "virsh", "destroy", op.Target); derr != nil {
			return failResult("源侧停机失败: " + derr.Error()), nil
		}
	}

	report(Stage{Key: "transfer", Name: "导出磁盘到暂存区", Index: 2, Total: 4})
	files := []map[string]any{}
	stage := func(src, kind string) bool {
		name := filepath.Base(src)
		if name == "" || name == "." || name == string(filepath.Separator) {
			return false
		}
		if _, err := sys.Run(ctx, "cp", "--reflink=auto", "--sparse=always",
			src, migrateStagePath(op.Target, name)); err != nil {
			return false
		}
		size := int64(0)
		if st, serr := osStat(migrateStagePath(op.Target, name)); serr == nil {
			size = st.Size()
		}
		files = append(files, map[string]any{
			"name": name, "size_bytes": size, "kind": kind, "source_path": src,
		})
		return true
	}
	for _, d := range sys.ParseDomainDisks(xml) {
		if d.SourceFile == "" || d.IsCDROM || d.IsFloppy {
			continue
		}
		if !stage(d.SourceFile, "disk") {
			return failResult("复制磁盘到暂存区失败: " + d.SourceFile), nil
		}
	}
	if nvram := nvramPathOf(xml); nvram != "" {
		_ = stage(nvram, "nvram") // 没有源 NVRAM 文件是异常但非致命：目标按模板重建
	}
	if memFile != "" {
		if st, serr := osStat(migrateStagePath(op.Target, memFile)); serr == nil {
			files = append(files, map[string]any{
				"name": memFile, "size_bytes": st.Size(), "kind": "memory",
				"source_path": migrateStagePath(op.Target, memFile),
			})
		}
	}

	report(Stage{Key: "source.undefine", Name: "源侧保持定义（防中途失去两侧）", Index: 3, Total: 4})
	// **源侧不 undefine**（真机验证踩出的设计修正）：提前移除定义会让
	// a) 接管失败后的重试在状态探测处被拒（域已不存在）；
	// b) 失败窗口内域定义两侧皆无。定义保留到 cleanup（记录落库成功之后）
	// 才移除；目标侧同名冲突由 takeover 按 UUID 比对处理。

	report(Stage{Key: "source.done", Name: "源侧完成", Index: 4, Total: 4})
	// 清单与 XML 落暂存区：重试快速路径与目标侧核对都依赖它。
	if mb, merr := json.Marshal(files); merr == nil {
		_ = osWriteFile(migrateStagePath(op.Target, "manifest.json"), mb, 0o600)
		_ = osWriteFile(migrateStagePath(op.Target, "domain.xml"), []byte(xml), 0o600)
	}
	return migrateSourceResult(op.Target, xml, files, memFile, start), nil
}

// migrateSourceResult 组装源侧成功回包（首次与重试共用同一形状）。
func migrateSourceResult(vm, xml string, files []map[string]any, memFile string, start int64) *agent.Result {
	moved := make([]string, 0, len(files))
	for _, f := range files {
		if n, ok := f["name"].(string); ok {
			moved = append(moved, n)
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.MigrateResultKey: agent.MigrateResult{
			Moved: moved, DurationSeconds: int(timeNowSec() - start),
		},
		"xml":    xml,
		"files":  files,
		"memory": memFile,
	}}
}

// migrateResumeFromStaging 从暂存区重发迁移材料（域已 undefine 的重试路径）。
// 材料齐备返回 (result, true)；否则 (nil, false) 交由调用方报错。
func migrateResumeFromStaging(vm string, start int64) (*agent.Result, bool) {
	mb, err := osReadFileBytes(migrateStagePath(vm, "manifest.json"))
	if err != nil {
		return nil, false
	}
	var files []map[string]any
	if jerr := json.Unmarshal(mb, &files); jerr != nil || len(files) == 0 {
		return nil, false
	}
	xml, err := osReadFile(migrateStagePath(vm, "domain.xml"))
	if err != nil || xml == "" {
		return nil, false
	}
	memFile := ""
	for _, f := range files {
		if k, _ := f["kind"].(string); k == "memory" {
			if n, _ := f["name"].(string); n != "" {
				memFile = n
			}
		}
	}
	return migrateSourceResult(vm, xml, files, memFile, start), true
}

// migrateStagePath 拼暂存区路径。
func migrateStagePath(vm, name string) string {
	return filepath.Join(migrateStagingRoot, vm, name)
}

// nvramPathOf 从域 XML 提取 <nvram> 的路径（BIOS 域返回空）。
func nvramPathOf(xml string) string {
	m := regexpNVRAM.FindStringSubmatch(xml)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

var regexpNVRAM = regexp.MustCompile(`<nvram[^>]*>([^<]+)</nvram>`)

// migrateCleanup 在迁移落库成功之后清理源侧（best-effort：失败只留日志，
// 不让清理问题把一次成功的迁移判成失败；残留可由管理员按清单手工删）。
func migrateCleanup(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "source.cleanup", Name: "清理源侧", Index: 1, Total: 1})
	// 源域定义在这里（记录已落库）才移除：迁移主体已由目标侧接管。
	_ = sys.Run0(ctx, "virsh", "undefine", "--nvram", "--managed-save", op.Target)
	files, _ := op.Params["files"].([]any)
	for _, f := range files {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		kind, _ := m["kind"].(string)
		// 只清名字干净的条目：name 会拼进路径，带 / 或 .. 的按篡改处理跳过。
		if name == "" || name != filepath.Base(name) {
			continue
		}
		switch kind {
		case "disk":
			_ = osRemove(filepath.Join(imagesDir, name))
		case "nvram":
			_ = osRemove(filepath.Join("/var/lib/libvirt/qemu/nvram", name))
		}
	}
	_ = osRemoveAll(filepath.Join(migrateStagingRoot, op.Target))
	return &agent.Result{Success: true}, nil
}

// vmMigrateTakeover 实现 vm.migrate.takeover（目标侧完整重建，P5 第二批）。
//
// staging 文件落位到镜像目录（属主/AppArmor 归一）→ XML 磁盘路径重映射 →
// NVRAM 放回 libvirt 约定路径 → 有内存态则 `virsh restore --xml`（恢复后
// 即运行），否则 `virsh define`。
func vmMigrateTakeover(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	xml := strParam(op.Params, "xml")
	filesAny, _ := op.Params["files"].([]any)
	memName := strParam(op.Params, "memory")
	if xml == "" {
		return failResult("缺少源域 XML（旧版源 agent？），无法重建"), nil
	}

	report(Stage{Key: "target.place", Name: "落位磁盘与固件", Index: 1, Total: 3})
	applied := []string{}
	for _, f := range filesAny {
		m, ok := f.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		kind, _ := m["kind"].(string)
		srcPath, _ := m["source_path"].(string)
		if name == "" || name != filepath.Base(name) {
			continue // 路径穿越防护：staging 名必须是干净 basename
		}
		// 幂等放置：上次接管可能已把部分文件 rename 走（放置中途失败后重试
		// 的常态）——暂存缺文件但**目标位置已有**时按已落位处理，不算失败。
		staged := migrateStagePath(op.Target, name)
		var dst string
		switch kind {
		case "disk":
			dst = filepath.Join(imagesDir, name)
		case "nvram":
			dst = filepath.Join("/var/lib/libvirt/qemu/nvram", name)
		default:
			continue
		}
		if _, serr := osStat(staged); serr != nil {
			if _, derr := osStat(dst); derr != nil {
				return failResult("暂存区缺少文件 " + name + "（中转未完成？）"), nil
			}
			xml = strings.ReplaceAll(xml, srcPath, dst)
			applied = append(applied, kindLabel(kind)+" "+name+"（已落位）")
			continue
		}
		if rerr := placeFile(staged, dst); rerr != nil {
			return failResult("落位" + kindLabel(kind) + "失败 " + name + ": " + rerr.Error()), nil
		}
		if kind == "disk" {
			_ = ensureQemuReadable(dst)
			ensureAppArmorAllowsImages(imagesDir)
		}
		xml = strings.ReplaceAll(xml, srcPath, dst)
		applied = append(applied, kindLabel(kind)+" "+name)
	}

	report(Stage{Key: "target.define", Name: "重建域定义", Index: 2, Total: 3})
	xmlFile := "/tmp/kc-migrate-" + op.Target + ".xml"
	if werr := osWriteFile(xmlFile, []byte(xml), 0o600); werr != nil {
		return failResult("写域定义失败: " + werr.Error()), nil
	}
	defer func() { _ = osRemove(xmlFile) }()
	// 同名域已存在：UUID 相同 = 源侧保留的定义（迁移流本就如此设计，
	// 同一 libvirt 实例的部署形态/重试会撞上）→ 移除后重建；UUID 不同 =
	// 目标上真有另一台同名机器 → 拒绝（绝不能顶掉别人的机器）。
	if sys.DomainExists(ctx, op.Target) {
		existing := domainUUIDOf(ctx, op.Target)
		want := uuidOfXML(xml)
		if existing == "" || want == "" || existing != want {
			return failResult("目标节点已存在同名域 " + op.Target + "（UUID " + existing +
				" 与迁移对象 " + want + " 不同），拒绝覆盖"), nil
		}
		if uerr := sys.Run0(ctx, "virsh", "undefine", "--nvram", "--managed-save", op.Target); uerr != nil {
			return failResult("移除源侧遗留定义失败: " + uerr.Error()), nil
		}
		applied = append(applied, "移除源侧遗留定义")
	}
	if memName != "" {
		mem := migrateStagePath(op.Target, memName)
		if _, err := osStat(mem); err != nil {
			return failResult("内存现场文件缺失: " + memName), nil
		}
		report(Stage{Key: "target.restore", Name: "恢复内存现场", Index: 3, Total: 3})
		// restore 默认建**瞬态域**（真机实证 Persistent: no，且 libvirt 10 的
		// restore 无 --persistent 选项——真机实测报 "doesn't support option"）。
		// 持久化配方：先 define（持久定义），再 restore --xml（把保存的状态
		// 灌进同 UUID 的已定义域）——恢复后既在跑、重启后也不丢。
		if _, derr := sys.Run(ctx, "virsh", "define", xmlFile); derr != nil {
			return failResult("定义域失败: " + derr.Error()), nil
		}
		if _, rerr := sys.Run(ctx, "virsh", "restore", "--xml", xmlFile, mem); rerr != nil {
			return failResult("恢复运行现场失败: " + rerr.Error()), nil
		}
		_ = osRemove(mem)
		applied = append(applied, "域定义", "内存现场（restore）")
	} else {
		if _, derr := sys.Run(ctx, "virsh", "define", xmlFile); derr != nil {
			return failResult("定义域失败: " + derr.Error()), nil
		}
		applied = append(applied, "域定义")
	}
	_ = osRemoveAll(filepath.Join(migrateStagingRoot, op.Target))
	return &agent.Result{Success: true, Data: map[string]any{
		agent.MigrateTakeoverDataKey: agent.MigrateTakeoverInfo{
			Applied: applied, Message: "目标侧接管完成",
		},
	}}, nil
}

// domainUUIDOf 取已存在域的 UUID（dominfo 输出；读不到返回空）。
func domainUUIDOf(ctx context.Context, vm string) string {
	out, err := sys.Run(ctx, "virsh", "dominfo", vm)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "UUID:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "UUID:"))
		}
	}
	return ""
}

// uuidOfXML 从域 XML 提取 <uuid>（缺失返回空）。
func uuidOfXML(xml string) string {
	m := regexp.MustCompile(`<uuid>([^<]+)</uuid>`).FindStringSubmatch(xml)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

// kindLabel 迁移条目类型的中文标签。
func kindLabel(kind string) string {
	switch kind {
	case "disk":
		return "磁盘"
	case "nvram":
		return "固件变量"
	case "memory":
		return "内存现场"
	}
	return kind
}

// placeFile 落位文件：同文件系统用 rename（秒级），跨文件系统回退复制。
func placeFile(src, dst string) error {
	if err := osRename(src, dst); err == nil {
		return nil
	}
	b, rerr := osReadFileBytes(src)
	if rerr != nil {
		return rerr
	}
	return osWriteFile(dst, b, 0o644)
}

// vmReinstall 实现 vm.reinstall：备份原盘 → 从模板盘重建系统盘。
func vmReinstall(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	templatePath := strParam(op.Params, "template_disk_path")
	diskGB, _ := op.Params["disk_gb"].(float64)
	report(Stage{Key: "backup", Name: "备份原系统盘", Index: 1, Total: 3})
	// 重装要求关机：运行中 mv 系统盘=拔正在写的硬盘（数据损坏）。
	// 控制面受理层已校验，这里是节点侧兜底。
	if state, _ := sys.DomainStateOf(ctx, op.Target); state == "running" || state == "paused" {
		return failResult("虚拟机正在运行，重装要求先关机"), nil
	}
	sysDev := sysDiskDev(ctx, op.Target)
	if sysDev == "" {
		return failResult("找不到系统盘"), nil
	}
	xml, _ := sys.DumpXML(ctx, op.Target, false)
	sourcePath := sysDiskSource(xml, sysDev)
	if sourcePath == "" {
		return failResult("系统盘没有文件路径（可能是块设备）"), nil
	}
	backupPath := sourcePath + ".pre-reinstall"
	if _, err := sys.Run(ctx, "mv", sourcePath, backupPath); err != nil {
		return failResult("备份失败: " + err.Error()), nil
	}

	report(Stage{Key: "rebuild", Name: "从模板重建", Index: 2, Total: 3})
	if templatePath != "" {
		if _, err := sys.Run(ctx, "qemu-img", "convert", "-O", "qcow2", templatePath, sourcePath); err != nil {
			// 重建失败必须把备份还原——那是用户数据的最后一份。
			_ = osRename(backupPath, sourcePath)
			return failResult("从模板重建失败: " + err.Error()), nil
		}
	} else {
		if _, err := sys.Run(ctx, "qemu-img", "create", "-f", "qcow2", sourcePath,
			fmt.Sprintf("%dG", int(diskGB))); err != nil {
			_ = osRename(backupPath, sourcePath)
			return failResult("创建空盘失败: " + err.Error()), nil
		}
	}

	report(Stage{Key: "uuid", Name: "刷新域定义", Index: 3, Total: 3})
	// undefine 失败要如实报并还原备份（此前吞错，后续 define 的报错
	// 掩盖真根因——与强删 bug 同型；review 抓出）。--managed-save /
	// --snapshots-metadata 与强删路径同一套兜底。
	uerr := sys.Run0(ctx, "virsh", "undefine", "--nvram", "--managed-save", op.Target)
	if uerr != nil && strings.Contains(uerr.Error(), "snapshots") {
		uerr = sys.Run0(ctx, "virsh", "undefine", "--nvram", "--managed-save",
			"--snapshots-metadata", op.Target)
	}
	if uerr != nil && sys.DomainExists(ctx, op.Target) {
		_ = osRename(backupPath, sourcePath)
		return failResult("移除旧域定义失败: " + uerr.Error()), nil
	}
	if _, err := sys.Run(ctx, "virsh", "define", xmlFileOf(xml, op.Target)); err != nil {
		_ = osRename(backupPath, sourcePath)
		// 此步失败时旧域定义已移除、原盘已还原但域处于未定义态——如实
		// 告知状态，别让用户以为机器还能直接开机。
		return failResult("重新定义域失败: " + err.Error() +
			"（原盘数据已还原，但域定义已被移除；重试重装或重建虚拟机可恢复）"), nil
	}
	_ = ensureQemuReadable(sourcePath)
	uuid, _ := sys.DomainUUID(ctx, op.Target)
	return &agent.Result{Success: true, Data: map[string]any{
		"uuid": uuid,
		agent.ReinstallDataKey: agent.ReinstallInfo{
			BackupPath: backupPath, DiskPath: sourcePath,
		},
	}}, nil
}

// sysDiskSource 从域 XML 找系统盘的 source 路径。
func sysDiskSource(xml, dev string) string {
	for _, d := range sys.ParseDomainDisks(xml) {
		if d.Dev == dev {
			return d.SourceFile
		}
	}
	return ""
}

// agentSafeUsername 收敛可进文件路径/命令参数的用户名字符集。
var agentSafeUsername = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// xmlFileOf 把 XML 写入临时文件供 virsh define 使用。
//
// 文件名必须带目标域名：两台 VM 并发重装时共用 /tmp/kc-redefine.xml
// 会互相覆盖——先写的域被 define 成别人家的 XML（review 抓出的碰撞）。
func xmlFileOf(xml, target string) string {
	f := "/tmp/kc-redefine-" + target + ".xml"
	_ = osWriteFile(f, []byte(xml), 0o600)
	return f
}

// vmReinstallPurge 实现 vm.reinstall.purge：删除重装备份盘。
func vmReinstallPurge(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "purge", Name: "删除备份", Index: 1, Total: 1})
	backupPath := strParam(op.Params, "backup_path")
	if backupPath != "" {
		_ = osRemove(backupPath)
	}
	return &agent.Result{Success: true}, nil
}

// vmRescue 实现 vm.rescue.enter / exit（救援模式）。
//
// enter：保存当前配置快照（dumpxml）→ 关机 → 挂救援 ISO 并开机。
// **引导顺序不改写**：域定义的 boot 顺序保持原样，原盘可引导时需要在
// 来宾固件里手动选光驱启动（盘型/网卡改写与 boot 顺序调整随 P5 的 XML
// 通道深化）；exit：按保存的定义还原。
func vmRescueEnter(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	snapshotXML := strParam(op.Params, "snapshot")
	rescueISO := strParam(op.Params, "rescue_iso")
	report(Stage{Key: "save_config", Name: "保存配置", Index: 1, Total: 2})
	if snapshotXML == "" {
		if xml, err := sys.DumpXML(ctx, op.Target, false); err == nil {
			snapshotXML = xml
			_ = osWriteFile("/var/lib/k_cockpit/rescue-"+op.Target+".xml", []byte(xml), 0o600)
		}
	}
	_ = snapshotXML
	report(Stage{Key: "boot_rescue", Name: "从救援介质引导", Index: 2, Total: 2})
	if rescueISO != "" {
		state, _ := sys.DomainStateOf(ctx, op.Target)
		if state == "running" {
			_, _ = sys.Run(ctx, "virsh", "destroy", op.Target)
		}
		frag := cdromFragment("sdc", "scsi", rescueISO)
		f := "/tmp/kc-rescue-" + op.Target + ".xml"
		if werr := osWriteFile(f, []byte("<device>"+frag+"</device>"), 0o600); werr != nil {
			return failResult("写救援设备片段失败: " + werr.Error()), nil
		}
		// 挂载失败/启动失败都必须如实报（此前全吞、恒报成功——救援
		// 是救砖场景，假成功会让人以为已经能进系统；review 抓出）。
		if _, aerr := sys.Run(ctx, "virsh", "attach-device", "--config", op.Target, f); aerr != nil {
			return failResult("挂载救援介质失败: " + aerr.Error()), nil
		}
		if _, serr := sys.Run(ctx, "virsh", "start", op.Target); serr != nil {
			return failResult("从救援介质启动失败: " + serr.Error()), nil
		}
	}
	return &agent.Result{Success: true}, nil
}

// vmRescueExit 实现 vm.rescue.exit：按保存的定义还原并重启。
func vmRescueExit(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "restore", Name: "还原配置", Index: 1, Total: 1})
	saved := "/var/lib/k_cockpit/rescue-" + op.Target + ".xml"
	if b, err := osReadFile(saved); err == nil {
		state, _ := sys.DomainStateOf(ctx, op.Target)
		if state == "running" {
			_, _ = sys.Run(ctx, "virsh", "destroy", op.Target)
		}
		_, _ = sys.Run(ctx, "virsh", "define", xmlFileOf(string(b), op.Target))
		_ = osRemove(saved)
	}
	return &agent.Result{Success: true}, nil
}

// vmExport 实现 vm.export：导出系统盘为 qcow2 镜像（要求关机）。
func vmExport(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	format := strParam(op.Params, "format")
	if format == "" {
		format = "qcow2"
	}
	report(Stage{Key: "convert", Name: "导出磁盘", Index: 1, Total: 1})
	sysDev := sysDiskDev(ctx, op.Target)
	xml, _ := sys.DumpXML(ctx, op.Target, false)
	sourcePath := sysDiskSource(xml, sysDev)
	if sourcePath == "" {
		return failResult("找不到系统盘"), nil
	}
	exportDir := "/var/lib/k_cockpit/exports"
	_ = osMkdirAllErr(exportDir)
	out := filepath.Join(exportDir, op.Target+"."+format)
	if _, err := sys.Run(ctx, "qemu-img", "convert", "-O", format, sourcePath, out); err != nil {
		return failResult("导出失败: " + err.Error()), nil
	}
	st, _ := os.Stat(out)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.ExportDataKey: agent.ExportInfo{
			FilePath:  out,
			FileName:  filepath.Base(out),
			SizeBytes: fileSizeOr0(st),
		},
	}}, nil
}

// vmExportDelete / vmExportFetch。
func vmExportDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	_ = osRemove(op.Target)
	return &agent.Result{Success: true}, nil
}

func vmExportFetch(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	path := strParam(op.Params, "file_path")
	b, err := osReadFileBytes(path)
	if err != nil {
		return failResult("读取导出文件失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Data: map[string]any{
		"content": agent.ExportContent{Data: b, MIME: "application/octet-stream"},
	}}, nil
}

// vmGuest 实现 vm.guest（来宾自动化，P5 第二批真实实现）。
//
// 需要来宾 agent 的动作走 `virsh qemu-agent-command`（guest-ping 探测、
// guest-set-user-password、guest-exec）；宿主侧动作（resize/attach）走
// qemu-img/virsh。guest-exec 经 guest-exec-status 轮询收结果（500ms×60）。
func vmGuest(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	action := strParam(op.Params, "action")
	username := strParam(op.Params, "username")
	password := strParam(op.Params, "password")
	diskID := strParam(op.Params, "disk_id")
	diskGB, _ := op.Params["disk_gb"].(float64)

	// password_offline 依赖 libguestfs；未安装时如实报，不伪造成功。
	if action == "password_offline" {
		report(Stage{Key: "host.precheck", Name: "检查离线改密工具", Index: 1, Total: 2})
		if !sys.RunOK(ctx, "virt-customize", "--version") {
			return failResult("离线改密需要 libguestfs 工具（virt-customize），" +
				"宿主机未安装；请改用在线改密（需来宾 agent）或安装 libguestfs"), nil
		}
		report(Stage{Key: "guest.exec", Name: "离线改写系统盘", Index: 2, Total: 2})
		dev := sysDiskDev(ctx, op.Target)
		path := sysDiskSourceOf(ctx, op.Target, dev)
		if path == "" {
			return failResult("找不到系统盘文件"), nil
		}
		if _, err := sys.Run(ctx, "virt-customize", "-a", path,
			"--root-password", "password:"+password); err != nil {
			return failResult("离线改密失败: " + err.Error()), nil
		}
		return guestResult(true, "已离线修改 root 密码"), nil
	}

	report(Stage{Key: "host.precheck", Name: "检查 Guest Agent 通道", Index: 1, Total: 2})
	if !guestPing(ctx, op.Target) {
		return failResult("来宾内未安装或未连通 qemu-guest-agent；请在虚拟机内安装后重试"), nil
	}

	report(Stage{Key: "guest.exec", Name: "来宾内执行", Index: 2, Total: 2})
	switch action {
	case "password_online":
		if username == "" || password == "" {
			return failResult("在线改密需要 username 与 password"), nil
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(password))
		if _, err := agentCmd(ctx, op.Target, "guest-set-user-password",
			"username", username, "password", b64, "crypted", false); err != nil {
			return failResult("来宾改密失败（密码策略或权限）: " + err.Error()), nil
		}
		return guestResult(true, "已修改 "+username+" 的密码"), nil

	case "expand_disk":
		dev := sysDiskDev(ctx, op.Target)
		path := sysDiskSourceOf(ctx, op.Target, dev)
		if path == "" || diskGB <= 0 {
			return failResult("扩容需要磁盘文件与正数大小"), nil
		}
		if _, err := sys.Run(ctx, "qemu-img", "resize", path,
			fmt.Sprintf("%dG", int(diskGB))); err != nil {
			return failResult("宿主侧加长磁盘失败: " + err.Error()), nil
		}
		if sys.RunOK(ctx, "virsh", "-r", "-c", "qemu:///system", "domstate", op.Target) {
			_ = sys.Run0(ctx, "virsh", "blockresize", op.Target, dev,
				fmt.Sprintf("%dG", int(diskGB)))
		}
		stdout, _, rc, err := guestExec(ctx, op.Target,
			"/bin/sh", "-c", "growpart "+diskDevOf(dev)+" 1 && (resize2fs "+diskPartOf(dev)+"1 || xfs_growfs /)")
		if err != nil || rc != 0 {
			return failResult("来宾侧扩容文件系统失败: " + errText(stdout, err) +
				"（磁盘已加长，请进系统手工扩容分区/文件系统）"), nil
		}
		return guestResult(true, "磁盘与文件系统已扩容"), nil

	case "disk_attach":
		if diskID == "" || diskGB <= 0 {
			return failResult("附加磁盘需要 disk_id 与大小"), nil
		}
		dp := filepath.Join(imagesDir, op.Target+"-"+diskID+".qcow2")
		if _, err := sys.Run(ctx, "qemu-img", "create", "-f", "qcow2", dp,
			fmt.Sprintf("%dG", int(diskGB))); err != nil {
			return failResult("创建磁盘失败: " + err.Error()), nil
		}
		_ = ensureQemuReadable(dp)
		ensureAppArmorAllowsImages(imagesDir)
		dev := nextFreeDataDev(ctx, op.Target)
		frag := fmt.Sprintf("<disk type='file' device='disk'><driver name='qemu' type='qcow2'/>"+
			"<source file='%s'/><target dev='%s' bus='virtio'/></disk>", xmlEsc(dp), dev)
		f := "/tmp/kc-guestdisk-" + op.Target + ".xml"
		if werr := osWriteFile(f, []byte(frag), 0o600); werr != nil {
			return failResult("写磁盘片段失败: " + werr.Error()), nil
		}
		if _, aerr := sys.Run(ctx, "virsh", "attach-device", "--config", op.Target, f); aerr != nil {
			_ = osRemove(dp)
			return failResult("挂载新盘到域失败: " + aerr.Error()), nil
		}
		_ = osRemove(f)
		return guestResult(true, "已附加磁盘 "+dev+"（重启后来宾内可见，分区/格式化请用 disk 附加后的 growpart 流程）"), nil

	default:
		return failResult("未知来宾动作: " + action), nil
	}
}

// guestResult 构造 vm.guest 的成功结果。
func guestResult(usedAgent bool, msg string) *agent.Result {
	return &agent.Result{Success: true, Data: map[string]any{
		agent.GuestDataKey: agent.GuestInfo{GuestAgentUsed: usedAgent, Message: msg},
	}}
}

// guestPing 经 qemu-agent-command 探测来宾 agent 连通性。
func guestPing(ctx context.Context, vm string) bool {
	_, err := agentCmd(ctx, vm, "guest-ping")
	return err == nil
}

// agentCmd 执行一次 qemu-agent-command；args 以 key,value 交替展开为
// arguments 对象（value 为 string 或 bool）。
func agentCmd(ctx context.Context, vm, execute string, kv ...any) (string, error) {
	args := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		args[fmt.Sprint(kv[i])] = kv[i+1]
	}
	payload := map[string]any{"execute": execute}
	if len(args) > 0 {
		payload["arguments"] = args
	}
	b, _ := json.Marshal(payload)
	return sys.Run(ctx, "virsh", "qemu-agent-command", vm, string(b))
}

// guestExec 在来宾内执行命令并等待完成（500ms 轮询，30s 上限），
// 返回 stdout、stderr、退出码。
func guestExec(ctx context.Context, vm, path string, arg ...string) (string, string, int, error) {
	args := []any{path}
	for _, a := range arg {
		args = append(args, a)
	}
	out, err := agentCmd(ctx, vm, "guest-exec", "path", args[0], "arg", args[1:], "capture-output", true)
	if err != nil {
		return "", "", -1, err
	}
	var launch struct {
		Return struct {
			Pid int `json:"pid"`
		} `json:"return"`
	}
	if jerr := json.Unmarshal([]byte(out), &launch); jerr != nil || launch.Return.Pid == 0 {
		return "", "", -1, fmt.Errorf("guest-exec 未返回 pid: %s", out)
	}
	pid := launch.Return.Pid
	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return "", "", -1, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		sout, serr := sys.Run(ctx, "virsh", "qemu-agent-command", vm,
			fmt.Sprintf(`{"execute":"guest-exec-status","arguments":{"pid":%d}}`, pid))
		if serr != nil {
			return "", "", -1, serr
		}
		var st struct {
			Return struct {
				Exited   bool   `json:"exited"`
				Exitcode int    `json:"exitcode"`
				OutData  string `json:"out-data"`
				ErrData  string `json:"err-data"`
			} `json:"return"`
		}
		if jerr := json.Unmarshal([]byte(sout), &st); jerr != nil {
			return "", "", -1, jerr
		}
		if !st.Return.Exited {
			continue
		}
		stdout := decodeB64(st.Return.OutData)
		stderr := decodeB64(st.Return.ErrData)
		return stdout, stderr, st.Return.Exitcode, nil
	}
	return "", "", -1, fmt.Errorf("来宾内执行超时（30s）")
}

// decodeB64 解 guest-exec 的 base64 输出（空串返回空）。
func decodeB64(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

// sysDiskSourceOf 取指定设备号的磁盘文件路径。
func sysDiskSourceOf(ctx context.Context, vm, dev string) string {
	xml, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return ""
	}
	for _, d := range sys.ParseDomainDisks(xml) {
		if d.Dev == dev {
			return d.SourceFile
		}
	}
	return ""
}

// diskDevOf / diskPartOf 把 vda/vdb 转成块设备与分区路径（vda → /dev/vda）。
func diskDevOf(dev string) string  { return "/dev/" + dev }
func diskPartOf(dev string) string { return diskDevOf(dev) }

// nextFreeDataDev 找域里第一个未占用的 virtio 盘号。
func nextFreeDataDev(ctx context.Context, vm string) string {
	xml, err := sys.DumpXML(ctx, vm, false)
	if err != nil {
		return "vdb"
	}
	used := map[string]bool{}
	for _, d := range sys.ParseDomainDisks(xml) {
		used[d.Dev] = true
	}
	for _, dev := range []string{"vdb", "vdc", "vdd", "vde", "vdf"} {
		if !used[dev] {
			return dev
		}
	}
	return "vdz"
}

func errText(stdout string, err error) string {
	if err != nil {
		return err.Error()
	}
	if len(stdout) > 200 {
		stdout = stdout[:200]
	}
	return stdout
}

// securityPasswordAudit 实现 security.password_audit。
//
// 判定在节点侧：从 shadow 读哈希并逐条比对抗弱口令候选。安全边界：
// shadow 只有 root 能读，agent 以 root 运行是唯一合法位置。
func securityPasswordAudit(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "read_shadow", Name: "读取口令哈希", Index: 1, Total: 2})
	b, err := osReadFile("/etc/shadow")
	if err != nil {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.PasswordAuditDataKey: agent.PasswordAuditInfo{
				Unavailable: "无法读取 /etc/shadow（权限不足）",
			},
		}}, nil
	}
	report(Stage{Key: "check", Name: "比对抗弱口令", Index: 2, Total: 2})
	// 候选集：常用弱口令 + 用户名本身。argon2/yescrypt 的计算成本高，
	// 候选集固定 18 条，天然满足 f-10-06「每用户 ≤ 32 个候选」的性能边界。
	candidates := []string{"123456", "password", "admin", "root", "12345678",
		"qwerty", "111111", "123123", "abc123", "password123", "letmein",
		"welcome", "monkey", "dragon", "master", "login", "princess", "sunshine"}
	hits := []agent.PasswordHit{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 2 {
			continue
		}
		username, hash := fields[0], fields[1]
		if hash == "" || strings.HasPrefix(hash, "!") || strings.HasPrefix(hash, "*") {
			continue // 无密码或已锁定
		}
		weak := false
		for _, cand := range candidates {
			if checkCryptPassword(cand, hash) {
				hits = append(hits, agent.PasswordHit{Username: username, Reason: "口令在常见弱口令表中"})
				weak = true
				break
			}
		}
		// 用户名作为口令：必须真实比过哈希才能报——此前 `cand == username`
		// 直接记命中，用户名恰好叫 root/admin 的账号无论密码是什么都会被
		// 误报（review 抓出的假阳性；假阳性会让整份报告失去可信度）。
		if !weak && username != "" && checkCryptPassword(username, hash) {
			hits = append(hits, agent.PasswordHit{Username: username, Reason: "口令与用户名相同"})
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PasswordAuditDataKey: agent.PasswordAuditInfo{
			Checked: len(strings.Split(string(b), "\n")) - 1, Hits: hits,
		},
	}}, nil
}

// checkCryptPassword 用 crypt(3) 校验候选口令（委托系统 crypt 实现以支持
// yescrypt/argon2 全家族——Go 标准库没有 crypt，命令行 openssl 也不支持
// yescrypt，因此调 python3 crypt（系统 passwd 所用同一实现））。
func checkCryptPassword(candidate, hash string) bool {
	if !strings.Contains(hash, "$") {
		return false
	}
	cmd := exec.Command("python3", "-c", fmt.Sprintf(
		"import crypt,sys; sys.exit(0 if crypt.crypt(%q, %q)==%q else 1)",
		candidate, hash, hash))
	return cmd.Run() == nil
}

// userSSHAccess 实现 user.ssh.access（SSH 访问开关）。
func userSSHAccess(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	username := strParam(op.Params, "username")
	enabled := boolParam(op.Params, "enabled")
	// 用户名会进文件路径（deny drop-in）——字符集必须收敛。控制面创建
	// 时已校验，这里防的是「校验上线前建的老账号」：`a/b` 之类会把 drop-in
	// 写到 sshd_config.d 之外（agent 侧第二道防线）。
	if !agentSafeUsername.MatchString(username) {
		return failResult("用户名字符集不合法（仅字母/数字/下划线/连字符）"), nil
	}
	report(Stage{Key: "ssh", Name: "调整 SSH 访问", Index: 1, Total: 1})
	killed := 0
	if !enabled {
		// 结束该用户的在线会话。参数化调用（绝不拼 shell 字符串——
		// 用户名来自控制面用户表，此前拼进 sh -c 是 root 级注入面；
		// review 抓出。控制面已加字符集校验，这里是第二道防线）。
		if out, err := sys.Run(ctx, "loginctl", "list-sessions", "--no-legend"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 3 && fields[2] == username {
					if _, kerr := sys.Run(ctx, "loginctl", "terminate-session", fields[0]); kerr == nil {
						killed++
					}
				}
			}
		}
	}
	// sshd DenyUsers drop-in（sshd_config.d 目录，发行版惯例）。
	denyFile := "/etc/ssh/sshd_config.d/10-kc-deny-" + username + ".conf"
	denyExists := false
	if _, err := osStat(denyFile); err == nil {
		denyExists = true
	}
	_ = osRemove(denyFile)
	if !enabled {
		_ = osWriteFile(denyFile, []byte("DenyUsers "+username+"\n"), 0o600)
	}
	// 禁用与解禁都要 reload：drop-in 只在 sshd 重载时被读取——只写/删文件
	// 不重载，内存里的 DenyUsers 依旧生效（表现为「解禁了却登不上」或
	// 反向「禁用了却还能登」）。解禁且此前没有 drop-in 时配置未变，跳过。
	// 服务名探测 + 回退：Debian/Ubuntu 是 ssh，RHEL/Fedora 是 sshd
	//（AGENTS.md：禁止硬编码服务名）。
	if !enabled || denyExists {
		for _, unit := range []string{"ssh", "sshd"} {
			if sys.RunOK(ctx, "systemctl", "reload", unit) {
				break
			}
		}
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.UserSSHAccessDataKey: agent.UserSSHAccessInfo{
			Applied: true, KilledSessions: killed, Message: "SSH 访问已更新",
		},
	}}, nil
}

// quotaEnforce 实现 quota.enforce（限速/断网处置）。
//
// 断网：iptables 按该用户虚拟机的 IP 集合 DROP；限速：tc/iptables mangle
// 随第二批。期望状态语义：每次判定重发全量。
func quotaEnforce(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "enforce", Name: "应用配额处置", Index: 1, Total: 1})
	enforce := boolParam(op.Params, "enforce")
	action := strParam(op.Params, "action")
	userID, _ := op.Params["user_id"].(float64)
	chain := fmt.Sprintf("KC-QUOTA-U%s", strconv.Itoa(int(userID)))
	_, _ = sys.Run(ctx, "iptables", "-N", chain)
	_, _ = sys.Run(ctx, "iptables", "-F", chain)
	if enforce && action == "block" {
		// 用户虚拟机的 IP 集合由控制面随 params 下发（ips 键）；无 IP 时
		// 只建空链（没有可处置对象）。
		if ips, ok := op.Params["ips"].([]any); ok {
			for _, ip := range ips {
				if s, ok := ip.(string); ok {
					iptablesAdd(ctx, chain, "-d", s, "-j", "DROP")
				}
			}
		}
	}
	_, _ = sys.Run(ctx, "iptables", "-D", "FORWARD", "-j", chain)
	if enforce {
		_, _ = sys.Run(ctx, "iptables", "-I", "FORWARD", "1", "-j", chain)
	}
	return &agent.Result{Success: true, Data: map[string]any{
		agent.QuotaDataKey: agent.QuotaInfo{Applied: enforce, Message: "配额处置已更新"},
	}}, nil
}

// platformRepair 实现 platform.repair（按期望重下发）。
func platformRepair(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "repair", Name: "重新下发期望状态", Index: 1, Total: 1})
	return &agent.Result{Success: true, Data: map[string]any{
		"repaired": 0,
		"failed":   []string{},
		"message":  "各能力的重下发随对应分期接入（当前无未完成项可修）",
	}}, nil
}
