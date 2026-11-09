package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// reinstallFixture 是一套「重装就绪」的现场：关机的域、一个内容可辨认的
// 系统盘，以及能记录调用的假命令。
type reinstallFixture struct {
	// sourcePath 是重装前的系统盘（会被 mv 成 <path>.pre-reinstall）。
	sourcePath string
	// backupPath 是备份路径。
	backupPath string
	// callsPath 记录 qemu-img 的实际调用，用于断言「扩没扩容」。
	callsPath string
	// templatePath 是模板盘路径（内容无关，convert 由假命令接管）。
	templatePath string
}

const reinstallOldContent = "old-system-content"

// newReinstallFixture 铺现场。qemuImgScript 由各用例给，因为「扩不扩」正是
// 由 qemu-img 的当前容量决定的。
func newReinstallFixture(t *testing.T, qemuImgScript string) *reinstallFixture {
	t.Helper()

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "vm-demo.qcow2")
	if err := os.WriteFile(sourcePath, []byte(reinstallOldContent), 0o644); err != nil {
		t.Fatalf("写入系统盘失败: %v", err)
	}
	callsPath := filepath.Join(dir, "qemu-img.calls")
	templatePath := filepath.Join(dir, "tpl-ubuntu.qcow2")

	// 域 XML 只要能让 sysDiskDev / sysDiskSource 找到系统盘即可：重装路径
	// 读的是 <target dev> 与 <source file> 两处。
	xml := `<domain type='kvm'>
  <name>demo-vm</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='` + sourcePath + `'/>
      <target dev='vda' bus='virtio'/>
    </disk>
  </devices>
</domain>`

	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\ncase \"$*\" in\n" +
			"  *domstate*) echo 'shut off' ;;\n" +
			"  *dumpxml*) cat <<'XMLEOF'\n" + xml + "\nXMLEOF\n ;;\n" +
			"  *domuuid*) echo '11111111-2222-3333-4444-555555555555' ;;\n" +
			"  *) exit 0 ;;\nesac\n",
		"qemu-img": strings.ReplaceAll(qemuImgScript, "@CALLS@", callsPath),
	})

	return &reinstallFixture{
		sourcePath:   sourcePath,
		backupPath:   sourcePath + ".pre-reinstall",
		callsPath:    callsPath,
		templatePath: templatePath,
	}
}

// runReinstall 以 disk_gb 执行一次 vmReinstall。
func runReinstall(t *testing.T, f *reinstallFixture, diskGB float64) *agent.Result {
	t.Helper()
	res, err := vmReinstall(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"template_disk_path": f.templatePath,
			"disk_gb":            diskGB,
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmReinstall 返回 error: %v", err)
	}
	return res
}

func callsOf(t *testing.T, f *reinstallFixture) string {
	t.Helper()
	b, err := os.ReadFile(f.callsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("读取调用记录失败: %v", err)
	}
	return string(b)
}

// TestVMReinstallGrowsDiskToRequestedSize 覆盖「重装后容量与记录不一致」。
//
// 实机现象：disk_gb=10 的机器用 t-ubuntu 重装，任务成功、备份也在，但
// `virsh domblkinfo <vm> vda` 的 Capacity 是 3758096384（模板原大小 3.5G），
// 而控制面仍记 10G——配额按 10G 计费，用户实际只拿到 3.5G。
//
// 根因是模板分支只做 qemu-img convert，而 convert 只复制内容、不改虚拟容量。
// 这条用例要求：convert 之后必须把盘扩到请求的容量。
func TestVMReinstallGrowsDiskToRequestedSize(t *testing.T) {
	f := newReinstallFixture(t, `#!/bin/sh
case "$1" in
  convert) echo "convert" >> @CALLS@ ; : > "$5" ;;
  info) echo '{"virtual-size": 3758096384}' ;;
  resize) echo "resize $*" >> @CALLS@ ;;
esac
`)

	res := runReinstall(t, f, 10)
	if !res.Success {
		t.Fatalf("重装应成功: %s", res.Message)
	}

	calls := callsOf(t, f)
	if !strings.Contains(calls, "resize") || !strings.Contains(calls, "10G") {
		t.Errorf("模板重建后应扩容到 10G，qemu-img 实际调用: %q", calls)
	}
	// 备份要保留：它是用户原系统盘的最后一份。
	if _, err := os.Stat(f.backupPath); err != nil {
		t.Errorf("重装成后备份盘丢失: %v", err)
	}
}

// TestVMReinstallKeepsLargerDisk 覆盖「只扩不缩」。
//
// 模板比请求的容量更大时（例如用户把 disk_gb 从 20 改小到 10 再重装），
// 缩容会截断数据；正确行为是原样保留模板的容量。
func TestVMReinstallKeepsLargerDisk(t *testing.T) {
	f := newReinstallFixture(t, `#!/bin/sh
case "$1" in
  convert) echo "convert" >> @CALLS@ ; : > "$5" ;;
  info) echo '{"virtual-size": 21474836480}' ;;
  resize) echo "resize $*" >> @CALLS@ ;;
esac
`)

	res := runReinstall(t, f, 10)
	if !res.Success {
		t.Fatalf("重装应成功: %s", res.Message)
	}
	if calls := callsOf(t, f); strings.Contains(calls, "resize") {
		t.Errorf("盘已比请求的大时不得缩容，qemu-img 实际调用: %q", calls)
	}
}

// TestVMReinstallRestoresBackupOnConvertFailure 覆盖失败时的兜底。
//
// convert 失败必须把备份还原回原路径：留下一个半截盘等于把用户的数据
// 换成一个打不开的空壳，而那台机器看起来「重装过了」。
func TestVMReinstallRestoresBackupOnConvertFailure(t *testing.T) {
	f := newReinstallFixture(t, `#!/bin/sh
case "$1" in
  convert) echo "convert failed" >&2 ; exit 1 ;;
  info) echo '{"virtual-size": 3758096384}' ;;
  resize) echo "resize $*" >> @CALLS@ ;;
esac
`)

	res := runReinstall(t, f, 10)
	if res.Success {
		t.Fatal("convert 失败时任务必须失败")
	}
	if !strings.Contains(res.Message, "从模板重建失败") {
		t.Errorf("失败原因未指向模板重建: %q", res.Message)
	}
	// 原盘必须回到原位且内容未变。
	b, err := os.ReadFile(f.sourcePath)
	if err != nil {
		t.Fatalf("原盘未还原: %v", err)
	}
	if string(b) != reinstallOldContent {
		t.Errorf("原盘内容被破坏: %q", string(b))
	}
	if calls := callsOf(t, f); strings.Contains(calls, "resize") {
		t.Errorf("重建失败后不应再做扩容，qemu-img 实际调用: %q", calls)
	}
}
