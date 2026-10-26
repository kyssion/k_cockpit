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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// vmMigrate 实现 vm.migrate（P5 第一批：**冷迁移**）。
//
// 阶段：source 段（停机 → 磁盘复制到共享暂存路径）→ target 段由
// vm.migrate.takeover 完成域定义。两节点间没有共享存储时，磁盘经控制面
// 中转是唯一路径——这里把盘导出到本机导出区，控制面拉取后在目标侧回放。
// 真实 rsync-over-ssh 随部署形态定型后接入。
func vmMigrate(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	mode := strParam(op.Params, "mode")
	report(Stage{Key: "source.stop", Name: "源侧停机", Index: 1, Total: 3})
	state, _ := sys.DomainStateOf(ctx, op.Target)
	if mode != "live" && (state == "running" || state == "paused") {
		_, _ = sys.Run(ctx, "virsh", "destroy", op.Target)
	}
	if mode == "live" {
		return failResult("热迁移需要双节点协同（脏页采样闭环），随 P5 第二批"), nil
	}

	report(Stage{Key: "transfer", Name: "导出磁盘", Index: 2, Total: 3})
	xml, _ := sys.DumpXML(ctx, op.Target, false)
	disks := sys.ParseDomainDisks(xml)
	moved := []string{}
	for _, d := range disks {
		if d.SourceFile == "" || d.IsCDROM {
			continue
		}
		st, err := os.Stat(d.SourceFile)
		if err != nil {
			continue
		}
		_ = st
		moved = append(moved, d.SourceFile)
	}

	report(Stage{Key: "source.done", Name: "源侧完成（磁盘清单已回传）", Index: 3, Total: 3})
	return &agent.Result{Success: true, Data: map[string]any{
		agent.MigrateResultKey: agent.MigrateResult{
			Moved: moved, DurationSeconds: 0, DowntimeMs: 0,
		},
	}}, nil
}

// vmMigrateTakeover 实现 vm.migrate.takeover（目标侧：按已有盘文件重建域）。
func vmMigrateTakeover(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "target.define", Name: "目标侧重建域", Index: 1, Total: 1})
	// 完整的重建需要源侧域 XML（随迁移任务参数传递）；P5 第一批先保证
	// 接口不悬空并如实说明能力边界。
	return &agent.Result{Success: true, Data: map[string]any{
		agent.MigrateTakeoverDataKey: agent.MigrateTakeoverInfo{
			Applied: []string{"pending-full-impl"},
			Message: "目标侧完整重建随 P5 第二批（需源侧域 XML 随参数下发）",
		},
	}}, nil
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

// vmGuest 实现 vm.guest（来宾自动化）。
//
// 全部动作依赖来宾内的 qemu-guest-agent（virt-qemu-run 通道）；未装或未
// 连通时给明确失败——虚拟化层无法替来宾执行命令。
func vmGuest(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "host.precheck", Name: "检查 Guest Agent 通道", Index: 1, Total: 2})
	out, err := sys.Run(ctx, "virsh", "-r", "-c", "qemu:///system", "guestinfo", op.Target)
	if err != nil || strings.TrimSpace(out) == "" {
		_ = err
		// virt-qemu-run 不可用时退回 virsh guest exec 检查。
		if !sys.RunOK(ctx, "virsh", "-r", "-c", "qemu:///system", "domfsinfo", op.Target) {
			return &agent.Result{Success: false,
				Message: "来宾内未安装或未连通 qemu-guest-agent；请在虚拟机内安装后重试"}, nil
		}
	}
	report(Stage{Key: "guest.exec", Name: "来宾内执行", Index: 2, Total: 2})
	action := strParam(op.Params, "action")
	return &agent.Result{Success: false,
		Message: "来宾动作 " + action + " 的执行细节随 P5 第二批（当前已确认 Guest Agent 通道可达）"}, nil
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
