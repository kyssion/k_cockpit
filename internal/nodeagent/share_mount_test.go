package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// shareDomainXML 是一份没有 9p 共享的域定义。
const shareDomainXML = `<domain type='kvm'>
  <name>demo-vm</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/k_cockpit/images/demo.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
  </devices>
</domain>`

// shareEnv 铺一套假的 virsh：dumpxml 输出给定定义，define 把落盘内容另存
// 一份供断言，domstate 返回给定状态。
type shareEnv struct {
	root      string // 假「用户存储根」
	sharedDir string // 合法共享目录（在 root 之下）
	defineOut string // define 收到的 XML
	calls     string // virsh 调用记录
}

func newShareEnv(t *testing.T, domainXML, state string) *shareEnv {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "userdata")
	shared := filepath.Join(root, "shared", "docs")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatalf("创建共享目录失败: %v", err)
	}

	// 存 realpath：vmShareMount 用 EvalSymlinks 后的真实路径做前缀校验，
	// 而 macOS 的 t.TempDir() 在 /var → /private/var 之下，不解析就会
	// 永远对不上（这是测试环境的事实，不是产品行为差异）。
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("解析存储根失败: %v", err)
	}
	prevRoot := userdataRoot()
	userdataRootValue.Store(realRoot)
	t.Cleanup(func() { userdataRootValue.Store(prevRoot) })

	xmlPath := filepath.Join(dir, "domain.xml")
	if err := os.WriteFile(xmlPath, []byte(domainXML), 0o644); err != nil {
		t.Fatalf("写入域定义失败: %v", err)
	}
	env := &shareEnv{
		root:      realRoot,
		sharedDir: filepath.Join(realRoot, "shared", "docs"),
		defineOut: filepath.Join(dir, "defined.xml"),
		calls:     filepath.Join(dir, "virsh.calls"),
	}
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\nlast=\"\"\nfor a in \"$@\"; do last=\"$a\"; done\n" +
			"echo \"$*\" >> " + env.calls + "\n" +
			"case \"$*\" in\n" +
			"  *dumpxml*) cat " + xmlPath + " ;;\n" +
			"  *domstate*) echo '" + state + "' ;;\n" +
			"  *define*) cp \"$last\" " + env.defineOut + " ;;\n" +
			"esac\n",
	})
	return env
}

// defined 返回 define 收到的 XML（没调用过时为空串）。
func (e *shareEnv) defined(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.defineOut)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("读取已落盘的域定义失败: %v", err)
	}
	return string(b)
}

func (e *shareEnv) callLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(e.calls)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("读取调用记录失败: %v", err)
	}
	return string(b)
}

func mountShareOp(hostPath, tag string, extra map[string]any) agent.Operation {
	params := map[string]any{"action": "mount", "host_path": hostPath, "tag": tag}
	for k, v := range extra {
		params[k] = v
	}
	return agent.Operation{Target: "demo-vm", Params: params}
}

// TestShareMountWritesFilesystemIntoDomain 覆盖「任务成功但域里没有 filesystem」。
//
// 实机现象：`POST /vms/:id/shares` 任务 success、记录 active，但
// `virsh dumpxml --inactive` 里没有任何 <filesystem>——节点侧只做了路径校验
// 与 tag 存在性判断，从不改定义。用户在来宾里找不到挂载点，而"成功"让他以为
// 是自己 tag 用错了。
func TestShareMountWritesFilesystemIntoDomain(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "shut off")

	res, err := vmShareMount(context.Background(),
		mountShareOp(env.sharedDir, "kcshare", nil), func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("挂载应成功: %s", res.Message)
	}

	defined := env.defined(t)
	if defined == "" {
		t.Fatal("没有调用 virsh define：域定义没有被修改（这正是假成功）")
	}
	for _, want := range []string{
		"<filesystem type='mount' accessmode='mapped'>",
		"<source dir='" + env.sharedDir + "'/>",
		"<target dir='kcshare'/>",
		"</filesystem>",
	} {
		if !strings.Contains(defined, want) {
			t.Errorf("落盘的域定义缺少 %q:\n%s", want, defined)
		}
	}
	// 插在 </devices> 之前，且原设备不能被破坏。
	if strings.Index(defined, "<filesystem") > strings.Index(defined, "</devices>") {
		t.Errorf("filesystem 必须位于 </devices> 之前:\n%s", defined)
	}
	if !strings.Contains(defined, "dev='vda'") {
		t.Errorf("原有磁盘设备被破坏:\n%s", defined)
	}
	// 关机域不该提示重启。
	info, _ := res.Data[agent.ShareDataKey].(agent.ShareInfo)
	if len(info.Warnings) != 0 {
		t.Errorf("关机域不该有重启告警: %+v", info.Warnings)
	}
}

// TestShareMountHonorsAccessModeAndReadOnly 覆盖两个属性下发。
func TestShareMountHonorsAccessModeAndReadOnly(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "shut off")

	res, err := vmShareMount(context.Background(), mountShareOp(env.sharedDir, "ro-share",
		map[string]any{"security_model": "passthrough", "read_only": true}), func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("挂载应成功: err=%v res=%+v", err, res)
	}
	defined := env.defined(t)
	if !strings.Contains(defined, "accessmode='passthrough'") {
		t.Errorf("accessmode 未按 security_model 下发:\n%s", defined)
	}
	if !strings.Contains(defined, "<readonly/>") {
		t.Errorf("read_only=true 时应有 <readonly/>:\n%s", defined)
	}
}

// TestShareMountOnRunningDomainWarnsRestart 覆盖「运行中的域不会热插」。
//
// define 只改持久定义；运行中的域要么重启后才出现，要么永远不出现。假称
// 已生效会让用户去来宾里反复找。
func TestShareMountOnRunningDomainWarnsRestart(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "running")

	res, err := vmShareMount(context.Background(), mountShareOp(env.sharedDir, "kcshare", nil), func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("挂载应成功: err=%v res=%+v", err, res)
	}
	info, _ := res.Data[agent.ShareDataKey].(agent.ShareInfo)
	if !strings.Contains(info.Message, "重启") {
		t.Errorf("运行中的域应说明重启后生效，实际: %q", info.Message)
	}
	if len(info.Warnings) == 0 {
		t.Error("运行中的域应给出重启告警")
	}
}

// TestShareMountIsIdempotent 覆盖重复挂载：同 tag 同目录是安全的重复操作，
// 但不能再去 define 一次（那会无谓地重写定义并触发 libvirt 重新解析）。
func TestShareMountIsIdempotent(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "shut off")
	ctx := context.Background()

	if res, err := vmShareMount(ctx, mountShareOp(env.sharedDir, "kcshare", nil), func(Stage) {}); err != nil || !res.Success {
		t.Fatalf("首次挂载失败: err=%v res=%+v", err, res)
	}
	firstDefine := env.defined(t)
	if firstDefine == "" {
		t.Fatal("首次挂载应写定义")
	}

	// 把已落盘的定义当成节点上的现状，再挂一次。
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.defineOut), "domain.xml"), []byte(firstDefine), 0o644); err != nil {
		t.Fatalf("更新假域定义失败: %v", err)
	}
	_ = os.Remove(env.defineOut)

	res, err := vmShareMount(ctx, mountShareOp(env.sharedDir, "kcshare", nil), func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("重复挂载应幂等成功: err=%v res=%+v", err, res)
	}
	if got := env.defined(t); got != "" {
		t.Errorf("幂等路径不该再次 define，实际写入:\n%s", got)
	}
}

// TestShareMountRejectsTagPointingElsewhere 覆盖同名不同目录。
//
// 静默改成新目录会让已在用旧目录的来宾（下一次开机后）读到另一份数据，
// 而界面上没有任何变化——那是最难查的一类"数据好像不对"。
func TestShareMountRejectsTagPointingElsewhere(t *testing.T) {
	other := filepath.Join("/var/lib/k_cockpit/userdata", "other")
	xmlWithShare := strings.Replace(shareDomainXML, "</devices>",
		"<filesystem type='mount' accessmode='mapped'>\n"+
			"      <source dir='"+other+"'/>\n"+
			"      <target dir='kcshare'/>\n"+
			"    </filesystem>\n  </devices>", 1)
	env := newShareEnv(t, xmlWithShare, "shut off")

	res, err := vmShareMount(context.Background(), mountShareOp(env.sharedDir, "kcshare", nil), func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("同名 tag 指向另一个目录时必须拒绝")
	}
	if !strings.Contains(res.Message, "请先卸载") {
		t.Errorf("拒绝原因应给出出路: %q", res.Message)
	}
	if got := env.defined(t); got != "" {
		t.Error("被拒绝时不该改定义")
	}
}

// TestShareUnmountRemovesFilesystem 覆盖卸载：块必须真的从定义里消失。
func TestShareUnmountRemovesFilesystem(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "shut off")
	ctx := context.Background()

	if res, err := vmShareMount(ctx, mountShareOp(env.sharedDir, "kcshare", nil), func(Stage) {}); err != nil || !res.Success {
		t.Fatalf("挂载失败: err=%v res=%+v", err, res)
	}
	mounted := env.defined(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(env.defineOut), "domain.xml"), []byte(mounted), 0o644); err != nil {
		t.Fatalf("更新假域定义失败: %v", err)
	}
	_ = os.Remove(env.defineOut)

	res, err := vmShareMount(ctx, agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"action": "unmount", "tag": "kcshare"},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("卸载应成功: err=%v res=%+v", err, res)
	}
	after := env.defined(t)
	if after == "" {
		t.Fatal("卸载必须重写域定义")
	}
	if strings.Contains(after, "<filesystem") || strings.Contains(after, "kcshare") {
		t.Errorf("卸载后定义里仍有共享:\n%s", after)
	}
	if !strings.Contains(after, "</devices>") || !strings.Contains(after, "dev='vda'") {
		t.Errorf("卸载不该破坏其余设备定义:\n%s", after)
	}
}

// TestShareUnmountMissingTagFails 覆盖卸载不存在的 tag（DEF-R3-09 的既有约定）。
func TestShareUnmountMissingTagFails(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "shut off")

	res, err := vmShareMount(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{"action": "unmount", "tag": "ghost"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("卸载不存在的共享必须失败")
	}
	if !strings.Contains(res.Message, "没有名为 ghost") {
		t.Errorf("失败原因应点名该 tag: %q", res.Message)
	}
	if got := env.defined(t); got != "" {
		t.Error("失败时不该改定义")
	}
}

// TestShareMountRejectsPathOutsideRoot 覆盖安全边界：路径先过 realpath 与
// 前缀校验，越界时一次 virsh 都不该调。
func TestShareMountRejectsPathOutsideRoot(t *testing.T) {
	env := newShareEnv(t, shareDomainXML, "shut off")

	res, err := vmShareMount(context.Background(), mountShareOp("/etc", "kcshare", nil), func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success {
		t.Fatal("共享根目录之外的路径必须被拒绝")
	}
	if strings.Contains(env.callLog(t), "define") {
		t.Error("越界路径不该触发 define")
	}
}
