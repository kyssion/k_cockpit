package vm_test

import (
	"strings"
	"testing"

	"k_cockpit/internal/vm"
)

// TestRedactsConsolePassword 覆盖本接口最要紧的一步。
//
// libvirt 的定义里有控制台密码，而界面特意把它做成「只写不读」（R-005）。
// 原样返回这个定义，等于**从那道门把密码再送出去**——用户以为看不到，
// 而它就在这个接口的响应里。
func TestRedactsConsolePassword(t *testing.T) {
	in := `<graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1' passwd='s3cret'>`
	out, names := vm.ExportRedactForTest(in)

	if strings.Contains(out, "s3cret") {
		t.Fatalf("控制台密码被原样返回了:\n%s", out)
	}
	// **属性名要留下**：保留结构才能看出"这里有一个密码字段"，
	// 而全删掉会让用户以为面板把配置读丢了。
	if !strings.Contains(out, "passwd=") {
		t.Errorf("属性名应保留——排查时需要看出那里原本是什么字段:\n%s", out)
	}
	// 被替换掉的字段名要**列出来**：不说的话，用户看到 [已脱敏] 会以为
	// 面板把它读错了——而真相是它本来就在那里、只是没给他看。
	if !contains(names, "passwd") {
		t.Errorf("应列出 passwd，实际 %v", names)
	}
}

// TestKeepsNormalConfigIntact 覆盖**误伤**。
//
// 用户看到一处不该被遮的地方被遮了，会开始怀疑这个开关到底遮了什么。
// 而 key= / secret= 在 libvirt 定义里更常见的用法是**引用**而不是凭据
// （secret 的实际值在 libvirt 的 secret store 里，XML 里只有一个 uuid），
// 因此刻意不把它们纳入脱敏。
func TestKeepsNormalConfigIntact(t *testing.T) {
	in := `<domain type='kvm'><vcpu>2</vcpu>
  <secret type='ceph' uuid='a1b2c3d4-0000-0000-0000-000000000000'/>
  <disk><source file='/var/lib/libvirt/images/vm.qcow2'/><target dev='vda' bus='virtio'/></disk>
</domain>`
	out, names := vm.ExportRedactForTest(in)
	for _, keep := range []string{
		"<domain type='kvm'>", "<vcpu>", "virtio", "qcow2",
		"/var/lib/libvirt/images/", "uuid='a1b2c3d4",
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("普通配置被误伤：%q 不在了:\n%s", keep, out)
		}
	}
	if len(names) != 0 {
		t.Errorf("这段里没有凭据，不该有脱敏记录，实际 %v", names)
	}
}

// TestHandlesBothQuoteStyles 覆盖两种引号。
func TestHandlesBothQuoteStyles(t *testing.T) {
	in := `<graphics type='vnc' passwd="abc123"/><spice passwd='def456'/>`
	out, _ := vm.ExportRedactForTest(in)
	if strings.Contains(out, "abc123") || strings.Contains(out, "def456") {
		t.Fatalf("两种引号都该被处理:\n%s", out)
	}
	if strings.Count(out, "passwd=") != 2 {
		t.Errorf("两处属性名都该保留:\n%s", out)
	}
}

// TestMultipleOccurrences 覆盖多处敏感。
//
// 只替换第一处是最常见的实现疏漏，而它留下的那一处往往正是最后生效的那个。
func TestMultipleOccurrences(t *testing.T) {
	in := `<a passwd='one'/><b passwd='two'/><c password='three'/>`
	out, names := vm.ExportRedactForTest(in)
	for _, leak := range []string{"one", "two", "three"} {
		if strings.Contains(out, leak) {
			t.Errorf("第 %q 处未被脱敏:\n%s", leak, out)
		}
	}
	if !contains(names, "passwd") || !contains(names, "password") {
		t.Errorf("应列出两类字段，实际 %v", names)
	}
}

// TestPlaceholderHidesLength 覆盖占位符的选择。
//
// 等长星号会让**长度本身**成为信息——VNC 密码有 8 位上限，一串 8 个星号
// 与 3 个星号能让人看出用户设了多长。
func TestPlaceholderHidesLength(t *testing.T) {
	long, _ := vm.ExportRedactForTest(`<graphics passwd='abcdefgh'/>`)
	short, _ := vm.ExportRedactForTest(`<graphics passwd='ab'/>`)
	if long != short {
		t.Errorf("不同长度的值应产生相同的脱敏结果:\n  %q\n  %q", long, short)
	}
}

func TestEmptyIsSafe(t *testing.T) {
	out, names := vm.ExportRedactForTest("")
	if out != "" || len(names) != 0 {
		t.Errorf("空输入应当安全返回，实际 %q %v", out, names)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
