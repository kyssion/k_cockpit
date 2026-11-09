package nodeagent

import (
	"os"
	"strings"
	"testing"
)

const samplePubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIL9v0Zx3RLb7bT0dQ0k9xWJ4mS3nZt2yqk8wT1oV6uHd alice@lab"

// TestPreprocessSSHInjectUsesFileSelector 覆盖「注入 SSH 公钥必然失败」。
//
// 实机现象：选了「注入 SSH 公钥」的预处理 100% 失败，任务错误是
// `virt-customize: error: invalid ssh-inject selector '/tmp/kc-ssh-key-<n>.pub'`
// ——同时把同一任务里的其它预处理项一起打挂。
//
// 根因是选择器少了 `file:`：virt-customize 的 `--ssh-inject USER[:SELECTOR]`
// 在 SELECTOR 不以 `file:` 开头时，会把整段当成**公钥内容**去解析，而一串
// 路径显然不是公钥。这条用例把选择器形式与临时文件内容一起钉住。
func TestPreprocessSSHInjectUsesFileSelector(t *testing.T) {
	args, done, cleanup, err := preprocessArgs(
		templatesDir+"/demo-vm.qcow2",
		[]string{"inject_ssh_key", "reset_machine_id"},
		samplePubKey,
	)
	defer cleanup()
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}

	// 步骤清单要如实反映"安排了什么"：它会被写进任务的返回结果。
	if len(done) != 2 || done[0] != "inject_ssh_key" || done[1] != "reset_machine_id" {
		t.Errorf("已安排步骤 = %v", done)
	}

	selector := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--ssh-inject" {
			selector = args[i+1]
		}
	}
	if selector == "" {
		t.Fatalf("参数里没有 --ssh-inject: %v", args)
	}
	// 这一条就是实机报错的那个点：必须是 `root:file:<路径>`，不能是 `root:<路径>`。
	if !strings.HasPrefix(selector, "root:file:") {
		t.Fatalf("选择器 = %q, 期望 root:file:<路径>（少了 file: 会让 virt-customize 报 invalid ssh-inject selector）", selector)
	}

	path := strings.TrimPrefix(selector, "root:file:")
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("读取临时公钥文件失败: %v", rerr)
	}
	if string(b) != samplePubKey+"\n" {
		t.Errorf("临时公钥文件内容 = %q, 期望公钥原文加一个换行", string(b))
	}
	// 权限：文件里是用户的公钥，留在多用户宿主的 /tmp 里时不该更可读。
	if fi, serr := os.Stat(path); serr == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("临时公钥文件权限 = %v, 期望 0600", fi.Mode().Perm())
	}

	// 清理后文件必须消失：它是每次预处理都新建的，留下来会无限堆积。
	cleanup()
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("临时公钥文件未被清理: %v", serr)
	}
}

// TestPreprocessArgsWithoutSSHKeyHasNoSelector 覆盖「不注入公钥时不产生临时文件」。
//
// 只测注入路径的话，一个"总是注入 root:file:..."的实现也会通过——那会把
// 一个空的公钥文件塞进镜像的 authorized_keys。
func TestPreprocessArgsWithoutSSHKey(t *testing.T) {
	args, done, cleanup, err := preprocessArgs(
		templatesDir+"/demo-vm.qcow2",
		[]string{"reset_machine_id", "remove_cloud_init"},
		"",
	)
	defer cleanup()
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	for _, a := range args {
		if strings.Contains(a, "--ssh-inject") {
			t.Errorf("没选注入公钥却出现了 --ssh-inject: %v", args)
		}
	}
	if len(done) != 2 {
		t.Errorf("已安排步骤 = %v, 期望 2 项", done)
	}
}

// TestPreprocessArgsRejectsUnknownStep 覆盖未知步骤的明确失败。
//
// 静默跳过会让用户以为某项做过了：预处理是改镜像内容的操作，而"以为改过
// 其实没改"的后果要等到克隆出来的机器上用起来才发现。
func TestPreprocessArgsRejectsUnknownStep(t *testing.T) {
	args, _, cleanup, err := preprocessArgs(
		templatesDir+"/demo-vm.qcow2", []string{"banana"}, "")
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "不支持的预处理项") {
		t.Fatalf("未知步骤应报错，得到 err=%v args=%v", err, args)
	}
}
