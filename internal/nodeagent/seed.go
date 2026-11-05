// seed.go：首次启动初始化介质（F-2-17）。
//
// 创建虚拟机时把 cloud-init / cloudbase-init 需要的配置写进一张 ISO，
// 作为第二块光驱挂给域。没有这一步，界面上的「首次启动初始化 / 主机名 /
// 初始密码」只是三个落库字段——来宾第一次开机时什么都不会发生。
//
// 介质内容全部由纯函数生成，命令只负责打包；这样单元测试能覆盖真正的
// 业务规则（哪些字段进了 user-data），而不必依赖宿主机上的 ISO 工具链。
package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// seedSpec 是生成一次初始化介质所需的全部输入。
type seedSpec struct {
	Mode     string // nocloud / configdrive
	Name     string
	Hostname string
	Username string
	Password string
}

// needsSeed 报告该初始化方式是否需要生成介质。
//
// openwrt 走的是磁盘布局（ext4 / squashfs+overlay），不需要 cloud-init 介质；
// none 同理。
func (s seedSpec) needsSeed() bool {
	return s.Mode == "nocloud" || s.Mode == "configdrive"
}

func (s seedSpec) volumeID() string {
	if s.Mode == "configdrive" {
		return "config-2"
	}
	return "cidata"
}

// buildSeedFiles 返回介质内的文件内容（相对路径 → 内容）。
func buildSeedFiles(s seedSpec) (map[string]string, error) {
	if s.Name == "" {
		return nil, errors.New("缺少虚拟机名称，无法生成 instance-id")
	}
	host := strings.TrimSpace(s.Hostname)
	if host == "" {
		host = s.Name
	}
	switch s.Mode {
	case "nocloud":
		return map[string]string{
			"meta-data": fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", yamlQuote(s.Name), yamlQuote(host)),
			"user-data": noCloudUserData(host, s.Username, s.Password),
		}, nil
	case "configdrive":
		// cloudbase-init 从 ConfigDrive v2 的 meta_data.json 读取主机名与
		// 管理员凭据；user_data 保留 cloud-config 形态供其它插件消费。
		meta := map[string]any{
			"uuid":     s.Name,
			"name":     s.Name,
			"hostname": host,
		}
		if s.Username != "" {
			meta["admin_username"] = s.Username
		}
		if s.Password != "" {
			meta["admin_password"] = s.Password
		}
		b, err := json.Marshal(meta)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"openstack/latest/meta_data.json": string(b),
			"openstack/latest/user_data":      "#cloud-config\nhostname: " + host + "\n",
		}, nil
	}
	return nil, fmt.Errorf("未知的初始化方式 %q", s.Mode)
}

func noCloudUserData(host, username, password string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("hostname: " + yamlQuote(host) + "\n")
	b.WriteString("fqdn: " + yamlQuote(host) + "\n")
	b.WriteString("manage_etc_hosts: true\n")
	if password != "" {
		b.WriteString("ssh_pwauth: true\n")
		b.WriteString("users:\n  - default\n")
		if username != "" {
			b.WriteString("  - name: " + yamlQuote(username) + "\n")
			b.WriteString("    sudo: ALL=(ALL) NOPASSWD:ALL\n")
			b.WriteString("    lock_passwd: false\n")
			// plain_text_passwd 让 cloud-init 自己在来宾内做哈希；把宿主机
			// 生成的哈希写进 ISO 会与来宾的算法口径耦合。
			b.WriteString("    plain_text_passwd: " + yamlQuote(password) + "\n")
			b.WriteString("    shell: /bin/bash\n")
		}
		b.WriteString("chpasswd: { expire: false }\n")
	}
	return b.String()
}

// yamlQuote 用单引号包住标量，按 YAML 规则把内部单引号翻倍。
func yamlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// seedISOTool 探测宿主机上的 ISO 打包工具。
//
// 用 LookPath 而不是"跑一次看看报什么错"：命令失败的 stderr 各发行版不同，
// 拿它区分"工具不存在"和"打包失败"会把真正的错误吞掉。
func seedISOTool() (path string, prefix []string, err error) {
	for _, tool := range []string{"genisoimage", "mkisofs"} {
		if p, e := exec.LookPath(tool); e == nil {
			return p, nil, nil
		}
	}
	if p, e := exec.LookPath("xorriso"); e == nil {
		return p, []string{"-as", "mkisofs"}, nil
	}
	return "", nil, errors.New("缺少 ISO 生成工具（genisoimage / mkisofs / xorriso），无法创建初始化介质")
}

// prepareSeedISO 生成并挂接初始化介质，返回 ISO 路径。
func prepareSeedISO(ctx context.Context, s seedSpec) (string, error) {
	files, err := buildSeedFiles(s)
	if err != nil {
		return "", err
	}
	tool, prefix, err := seedISOTool()
	if err != nil {
		return "", err
	}

	dir, err := os.MkdirTemp("", "kc-seed-"+sanitizeFileName(s.Name)+"-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(imagesDir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(imagesDir, s.Name+"-seed.iso")
	args := append([]string{}, prefix...)
	args = append(args, "-output", out, "-volid", s.volumeID(),
		"-joliet", "-rock", dir)
	if _, err := sys.Run(ctx, tool, args...); err != nil {
		_ = os.Remove(out)
		return "", fmt.Errorf("生成初始化介质失败: %w", err)
	}
	_ = ensureQemuReadable(out)
	return out, nil
}

// attachSeedISO 把初始化介质挂成第二块光驱（sdb）。
//
// 安装 ISO 由控制面创建完成后挂到 sda，两者不能抢同一个设备名；
// 复用既有光驱执行器，保证路径/权限/临时文件处理只有一份实现。
func attachSeedISO(ctx context.Context, vmName, isoPath string) error {
	res, err := vmCDROMApply(ctx, agent.Operation{
		Target: vmName,
		Params: map[string]any{
			"action":   "attach",
			"bus":      "sata",
			"order_no": float64(1),
			"iso_path": isoPath,
		},
	}, func(Stage) {})
	if err != nil {
		return err
	}
	if !res.Success {
		return errors.New(res.Message)
	}
	return nil
}

// rollbackCreatedDomain 撤销一次已经 define 的创建（含已建磁盘与介质）。
//
// 创建后半段失败时不能只返回错误：域已经存在，控制面却认为创建失败，
// 用户重试会撞"虚拟化层已存在同名域"，而界面上看不到这台机器。
func rollbackCreatedDomain(ctx context.Context, name string, paths []string, extra string) {
	_, _ = sys.Run(ctx, "virsh", "undefine", "--nvram", "--managed-save", name)
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(p)
		}
	}
	if extra != "" {
		_ = os.Remove(extra)
	}
}

// primaryMACOf 读域定义里的第一块网卡 MAC（libvirt 在 define 时生成）。
func primaryMACOf(ctx context.Context, vmName string) string {
	xml, err := sys.DumpXML(ctx, vmName, false)
	if err != nil {
		return ""
	}
	i := strings.Index(xml, "<mac ")
	if i < 0 {
		return ""
	}
	end := strings.Index(xml[i:], "/>")
	if end < 0 {
		return ""
	}
	return attrValue(xml[i:i+end+2], "address")
}
