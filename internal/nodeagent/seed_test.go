package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

func TestBuildNoCloudSeedFiles(t *testing.T) {
	files, err := buildSeedFiles(seedSpec{
		Mode: "nocloud", Name: "vm-web", Hostname: "web-01",
		Username: "root", Password: "p'ass",
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	meta := files["meta-data"]
	if !strings.Contains(meta, "instance-id: 'vm-web'") || !strings.Contains(meta, "local-hostname: 'web-01'") {
		t.Errorf("meta-data 形状不对: %s", meta)
	}
	user := files["user-data"]
	for _, want := range []string{
		"#cloud-config", "hostname: 'web-01'", "ssh_pwauth: true",
		"name: 'root'", "plain_text_passwd: 'p''ass'", // YAML 单引号转义
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user-data 缺少 %q:\n%s", want, user)
		}
	}
}

func TestBuildNoCloudSeedWithoutPassword(t *testing.T) {
	files, _ := buildSeedFiles(seedSpec{Mode: "nocloud", Name: "vm-1"})
	if strings.Contains(files["user-data"], "plain_text_passwd") {
		t.Error("没有密码时不应写入凭据块")
	}
	if !strings.Contains(files["meta-data"], "local-hostname: 'vm-1'") {
		t.Error("主机名为空时应回退到虚拟机名")
	}
}

func TestBuildConfigDriveSeedFiles(t *testing.T) {
	files, err := buildSeedFiles(seedSpec{
		Mode: "configdrive", Name: "win-1", Hostname: "win-host",
		Username: "Administrator", Password: "s3cret",
	})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	meta := files["openstack/latest/meta_data.json"]
	for _, want := range []string{
		`"hostname":"win-host"`, `"admin_username":"Administrator"`, `"admin_password":"s3cret"`,
	} {
		if !strings.Contains(meta, want) {
			t.Errorf("meta_data.json 缺少 %q: %s", want, meta)
		}
	}
	if files["openstack/latest/user_data"] == "" {
		t.Error("ConfigDrive 缺少 user_data")
	}
}

func TestSeedNeedsAndVolumeID(t *testing.T) {
	for _, mode := range []string{"nocloud", "configdrive"} {
		if !(seedSpec{Mode: mode}).needsSeed() {
			t.Errorf("%s 应生成介质", mode)
		}
	}
	for _, mode := range []string{"none", "openwrt"} {
		if (seedSpec{Mode: mode}).needsSeed() {
			t.Errorf("%s 不应生成介质", mode)
		}
	}
	if got := (seedSpec{Mode: "configdrive"}).volumeID(); got != "config-2" {
		t.Errorf("ConfigDrive 卷标 = %q", got)
	}
	if got := (seedSpec{Mode: "nocloud"}).volumeID(); got != "cidata" {
		t.Errorf("NoCloud 卷标 = %q", got)
	}
}

func TestPrepareSeedISO(t *testing.T) {
	old := imagesDir
	imagesDir = t.TempDir()
	t.Cleanup(func() { imagesDir = old })

	logPath := filepath.Join(t.TempDir(), "iso.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"out=''; prev=''\n" +
		"for a in \"$@\"; do if [ \"$prev\" = '-output' ]; then out=\"$a\"; fi; prev=\"$a\"; done\n" +
		"for a in \"$@\"; do case \"$a\" in */kc-seed-*) find \"$a\" -type f >> " + logPath + " ;; esac; done\n" +
		"printf 'ISO' > \"$out\"\n"
	installFakeCommands(t, map[string]string{"genisoimage": script})

	path, err := prepareSeedISO(context.Background(), seedSpec{
		Mode: "nocloud", Name: "seed-vm", Hostname: "seed-host",
	})
	if err != nil {
		t.Fatalf("生成介质失败: %v", err)
	}
	defer func() { _ = os.Remove(path) }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ISO 未生成: %v", err)
	}

	raw, _ := os.ReadFile(logPath)
	log := string(raw)
	for _, want := range []string{"-volid cidata", "-joliet", "-rock", "user-data", "meta-data"} {
		if !strings.Contains(log, want) {
			t.Errorf("打包命令或介质内容缺少 %q:\n%s", want, log)
		}
	}
}

func TestPrimaryMACOf(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n  *dumpxml*) echo \"<domain><devices><interface type='bridge'><mac address='52:54:00:aa:bb:cc'/></interface></devices></domain>\" ;;\nesac\n",
	})
	got := primaryMACOf(context.Background(), "demo-vm")
	if got != "52:54:00:aa:bb:cc" {
		t.Errorf("MAC = %q", got)
	}
}

func TestVMInitReadyRequiresGuestAgent(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nexit 1\n",
	})
	res, err := vmInitReady(context.Background(), agent.Operation{Target: "demo-vm"}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("探测本身不应失败: %v %v", err, res)
	}
	info, _ := res.Data[agent.InitReadyDataKey].(agent.InitReadyInfo)
	if info.Ready {
		t.Error("没有 guest agent 时不应判定为就绪")
	}
	if !strings.Contains(info.Detail, "guest-agent") {
		t.Errorf("未说明原因: %s", info.Detail)
	}
}

func TestVMInitReadyDetectsCloudInitMarker(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "agent.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + logPath + "\n" +
		"case \"$*\" in\n" +
		"  *guest-ping*) echo '{\"return\":{}}' ;;\n" +
		"  *guest-file-open*) echo '{\"return\":7}' ;;\n" +
		"  *guest-file-close*) echo '{\"return\":{}}' ;;\n" +
		"esac\n"
	installFakeCommands(t, map[string]string{"virsh": script})

	res, err := vmInitReady(context.Background(), agent.Operation{Target: "demo-vm"}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("探测失败: %v %v", err, res)
	}
	info, _ := res.Data[agent.InitReadyDataKey].(agent.InitReadyInfo)
	if !info.Ready {
		t.Errorf("检测到 boot-finished 应判为就绪: %s", info.Detail)
	}
	raw, _ := os.ReadFile(logPath)
	if !strings.Contains(string(raw), "guest-file-close") {
		t.Error("打开的文件句柄未关闭")
	}
}
