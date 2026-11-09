package nodeagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

// useTempDiskDirs 把镜像根与清单根指到临时目录，并在用完后还原。
//
// imagesDir / vmMetaDir 是包级变量（生产值指向 /var/lib），测试必须换掉：
// 在开发机上往 /var/lib 写既不可行也不该留痕。用例因此不能并行。
func useTempDiskDirs(t *testing.T) (images, meta string) {
	t.Helper()
	root := t.TempDir()
	images = filepath.Join(root, "images")
	meta = filepath.Join(root, "vmmeta")
	oldImages, oldMeta := imagesDir, vmMetaDir
	imagesDir, vmMetaDir = images, meta
	t.Cleanup(func() {
		imagesDir, vmMetaDir = oldImages, oldMeta
	})
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatalf("创建临时镜像目录失败: %v", err)
	}
	return images, meta
}

// fakeVirshForDelete 铺一个「域定义已消失」的现场：dumpxml 输出为空
// （僵尸机的形态），dominfo 失败（域不在），其余命令一律成功。
func fakeVirshForDelete(t *testing.T) {
	t.Helper()
	installFakeCommands(t, map[string]string{
		"virsh": "#!/bin/sh\n" +
			"case \"$*\" in\n" +
			"  *dominfo*) exit 1 ;;\n" +
			"  *dumpxml*) exit 0 ;;\n" +
			"  *domstate*) echo 'shut off' ;;\n" +
			"esac\n" +
			"exit 0\n",
	})
}

// writeTempDisk 造一个真实文件（删除路径断言的是文件系统事实，不是命令调用）。
func writeTempDisk(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("disk"), 0o644); err != nil {
		t.Fatalf("创建磁盘文件 %s 失败: %v", path, err)
	}
}

// mustExist / mustNotExist 是两条最常用的文件系统断言。
func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s 应存在，但 stat 失败: %v（%s）", path, err, why)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s 应已被删除，但仍存在（%s）", path, why)
	}
}

// deleteWithDisksAction 以指定的 disk_action 执行一次 vmDelete（非 force）。
func deleteWithDisksAction(t *testing.T, name, diskAction string) *agent.Result {
	t.Helper()
	res, err := vmDelete(context.Background(), agent.Operation{
		Target: name,
		Params: map[string]any{"disk_action": diskAction},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmDelete 返回 error: %v", err)
	}
	return res
}

// TestVMCreateWritesDiskManifest 覆盖「创建时落盘磁盘清单」这条主路径。
//
// 缺陷现场：域定义被外部 `virsh undefine` 掉之后，删除路径再也读不到这台
// 机器有哪些盘——清单是那时唯一的依据，所以它必须在创建成功时就存在，
// 且包含系统盘与全部数据盘的绝对路径。
func TestVMCreateWritesDiskManifest(t *testing.T) {
	images, meta := useTempDiskDirs(t)
	installFakeCommands(t, map[string]string{
		// 同名域不存在（dominfo 失败）；dumpxml 给出网卡以便回填 MAC。
		"virsh": "#!/bin/sh\n" +
			"case \"$*\" in\n" +
			"  *dominfo*) exit 1 ;;\n" +
			"  *dumpxml*) printf '%s\\n' '<domain><devices><interface type=\"network\"><mac address=\"52:54:00:aa:bb:cc\"/></interface></devices></domain>' ;;\n" +
			"  *domuuid*) echo '11111111-2222-3333-4444-555555555555' ;;\n" +
			"esac\n" +
			"exit 0\n",
		// 假 qemu-img：不真的建盘，创建路径只关心"命令成功"。
		"qemu-img": "#!/bin/sh\nexit 0\n",
	})

	res, err := vmCreate(context.Background(), agent.Operation{
		Target: "demo-vm",
		Params: map[string]any{
			"vcpu":      float64(2),
			"memory_mb": float64(1024),
			"disk_gb":   float64(10),
			"data_disks": []dataDiskSpec{
				{SizeGB: 20, Format: "qcow2", DevHint: "vdb"},
			},
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("vmCreate 返回 error: %v", err)
	}
	if !res.Success {
		t.Fatalf("创建应成功: %s", res.Message)
	}

	manifestPath := filepath.Join(meta, "demo-vm.json")
	mustExist(t, manifestPath, "创建成功后必须有磁盘清单")

	raw, rerr := os.ReadFile(manifestPath)
	if rerr != nil {
		t.Fatalf("读取清单失败: %v", rerr)
	}
	var m vmDiskManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("清单不是合法 JSON: %v（内容: %s）", err, raw)
	}
	if m.Name != "demo-vm" {
		t.Errorf("清单里的名字不对: %q", m.Name)
	}
	if m.CreatedAt <= 0 {
		t.Errorf("清单缺少 created_at: %d", m.CreatedAt)
	}
	wantSys := filepath.Join(images, "demo-vm.qcow2")
	wantData := filepath.Join(images, "demo-vm-vdb.qcow2")
	if len(m.Disks) != 2 {
		t.Fatalf("清单应含系统盘 + 1 块数据盘，实际 %d 条: %v", len(m.Disks), m.Disks)
	}
	if m.Disks[0] != wantSys {
		t.Errorf("清单第一条应是系统盘 %q，实际 %q", wantSys, m.Disks[0])
	}
	if m.Disks[1] != wantData {
		t.Errorf("清单第二条应是数据盘 %q，实际 %q", wantData, m.Disks[1])
	}
}

// TestVMDeleteUsesManifestWhenDomainMissing 是本次缺陷的**复现用例**。
//
// 现场：域定义已被外部删除（dumpxml 为空）、创建时落过清单、两块盘真实
// 存在。删除任务必须按清单把两块盘都清掉，并删掉清单本身。
func TestVMDeleteUsesManifestWhenDomainMissing(t *testing.T) {
	images, meta := useTempDiskDirs(t)
	sysDisk := filepath.Join(images, "demo-vm.qcow2")
	dataDisk := filepath.Join(images, "demo-vm-vdb.qcow2")
	writeTempDisk(t, sysDisk)
	writeTempDisk(t, dataDisk)
	if err := writeDiskManifest("demo-vm", []string{sysDisk, dataDisk}); err != nil {
		t.Fatalf("预写清单失败: %v", err)
	}
	fakeVirshForDelete(t)

	res := deleteWithDisksAction(t, "demo-vm", "delete")
	if !res.Success {
		t.Fatalf("删除应成功: %s", res.Message)
	}
	if _, err := os.Stat(sysDisk); err == nil {
		t.Errorf("系统盘应被删除（缺陷现场），实际仍在: %s", sysDisk)
	}
	if _, err := os.Stat(dataDisk); err == nil {
		t.Errorf("数据盘应被删除，实际仍在: %s", dataDisk)
	}
	mustNotExist(t, filepath.Join(meta, "demo-vm.json"),
		"彻底删除后清单不应残留（否则同名重建后再删会信旧清单）")
}

// TestVMDeleteFallsBackToExactSystemDisk 覆盖旧机器（没有清单）的兜底。
//
// 关键约束：兜底只能按**精确文件名**删系统盘。数据盘文件名与"另一台机器
// 的系统盘"在命名上无法区分（VM 名可能长成 foo-vdb），所以
// `demo-vm-vdb.qcow2` 必须留下来，只能在结果里提示人工确认。
func TestVMDeleteFallsBackToExactSystemDisk(t *testing.T) {
	images, _ := useTempDiskDirs(t)
	sysDisk := filepath.Join(images, "demo-vm.qcow2")
	lookalike := filepath.Join(images, "demo-vm-vdb.qcow2")
	writeTempDisk(t, sysDisk)
	writeTempDisk(t, lookalike)
	fakeVirshForDelete(t)

	res := deleteWithDisksAction(t, "demo-vm", "delete")
	if !res.Success {
		t.Fatalf("删除应成功: %s", res.Message)
	}
	if _, err := os.Stat(sysDisk); err == nil {
		t.Errorf("无清单时也要按精确文件名删掉系统盘，实际仍在: %s", sysDisk)
	}
	if _, err := os.Stat(lookalike); err != nil {
		t.Errorf("数据盘形态的文件不得自动删除（可能是另一台机器的系统盘）: %s", lookalike)
	}
	note, _ := res.Data["message"].(string)
	if !strings.Contains(note, "数据盘可能残留") {
		t.Errorf("结果里应提示数据盘可能残留，实际: %q（Message=%q）", note, res.Message)
	}
	if !strings.Contains(note, "demo-vm-vdb.qcow2") {
		t.Errorf("提示里应指出具体文件，实际: %q", note)
	}
}

// TestVMDeleteKeepPreservesDisksAndManifest 覆盖「移入回收站」语义。
//
// keep 意味着磁盘**和清单**都留在原处：恢复回来的机器再次删除时还要用
// 清单（域 XML 里那时同样可能读不到磁盘）。
func TestVMDeleteKeepPreservesDisksAndManifest(t *testing.T) {
	images, meta := useTempDiskDirs(t)
	sysDisk := filepath.Join(images, "demo-vm.qcow2")
	dataDisk := filepath.Join(images, "demo-vm-vdb.qcow2")
	writeTempDisk(t, sysDisk)
	writeTempDisk(t, dataDisk)
	if err := writeDiskManifest("demo-vm", []string{sysDisk, dataDisk}); err != nil {
		t.Fatalf("预写清单失败: %v", err)
	}
	fakeVirshForDelete(t)

	res := deleteWithDisksAction(t, "demo-vm", "keep")
	if !res.Success {
		t.Fatalf("移入回收站应成功: %s", res.Message)
	}
	mustExist(t, sysDisk, "disk_action=keep 不得删盘")
	mustExist(t, dataDisk, "disk_action=keep 不得删盘")
	mustExist(t, filepath.Join(meta, "demo-vm.json"),
		"disk_action=keep 必须保留清单，恢复后的机器还要用")
}

// TestDataDiskResidueFilesIgnoresUnrelatedNames 锁住"只认 <vm>-<dev> 形态"。
//
// 镜像目录里同时有多台机器的盘：另一台机器 `demo-vm2.qcow2`、一份同名备份
// `demo-vm.qcow2.pre-reinstall` 都不该被当成 demo-vm 的数据盘。
func TestDataDiskResidueFilesIgnoresUnrelatedNames(t *testing.T) {
	images, _ := useTempDiskDirs(t)
	for _, name := range []string{
		"demo-vm-vdb.qcow2",
		"demo-vm-sda.raw",
		"demo-vm2.qcow2",
		"demo-vm.qcow2.pre-reinstall",
		"demo-vm-vdb.qcow2.pre-reinstall",
	} {
		writeTempDisk(t, filepath.Join(images, name))
	}

	got := dataDiskResidueFiles("demo-vm")
	if len(got) != 2 {
		t.Fatalf("应只识别出 2 个数据盘形态的文件，实际 %d 个: %v", len(got), got)
	}
	for _, p := range got {
		base := filepath.Base(p)
		if base != "demo-vm-vdb.qcow2" && base != "demo-vm-sda.raw" {
			t.Errorf("识别出不该匹配的文件: %s", base)
		}
	}
}

// TestVMDeleteIgnoresManifestForOtherName 覆盖清单名字被改坏/错配的情况。
//
// 清单内容与文件主名不一致时**不能**按里面的路径删——那可能是别人家的
// 清单，删掉就是删了另一台机器的盘。此时退到兜底（精确删系统盘）。
func TestVMDeleteIgnoresManifestWithMismatchedName(t *testing.T) {
	images, meta := useTempDiskDirs(t)
	victim := filepath.Join(images, "other-vm.qcow2")
	writeTempDisk(t, victim)
	sysDisk := filepath.Join(images, "demo-vm.qcow2")
	writeTempDisk(t, sysDisk)

	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatalf("创建清单目录失败: %v", err)
	}
	bad := `{"name":"other-vm","disks":["` + victim + `"],"created_at":1}`
	if err := os.WriteFile(filepath.Join(meta, "demo-vm.json"), []byte(bad), 0o600); err != nil {
		t.Fatalf("写入错配清单失败: %v", err)
	}
	fakeVirshForDelete(t)

	res := deleteWithDisksAction(t, "demo-vm", "delete")
	if !res.Success {
		t.Fatalf("删除应成功: %s", res.Message)
	}
	mustExist(t, victim, "清单名字与文件主名不符时不得按清单删盘（可能是别人家的盘）")
	mustNotExist(t, sysDisk, "错配清单应退到兜底：按精确文件名删自己的系统盘")
}

// TestManifestPathOutsideImagesDirIgnored 覆盖清单被篡改/损坏时越界删除。
//
// 清单由节点写入，但删除不可逆：即使清单里出现了镜像目录之外的绝对路径，
// 也必须忽略它并退回精确名兜底——不能让一个 JSON 文件变成删任意路径的能力。
func TestManifestPathOutsideImagesDirIgnored(t *testing.T) {
	images, _ := useTempDiskDirs(t)
	inside := filepath.Join(images, "demo.qcow2")
	writeTempDisk(t, inside)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "do-not-delete.txt")
	writeTempDisk(t, outside)

	if err := writeDiskManifest("demo", []string{inside, outside}); err != nil {
		t.Fatalf("写清单失败: %v", err)
	}
	fakeVirshForDelete(t)

	res := deleteWithDisksAction(t, "demo", "delete")
	if !res.Success {
		t.Fatalf("删除应成功: %s", res.Message)
	}
	mustNotExist(t, inside, "镜像目录内的磁盘应被删除")
	mustExist(t, outside, "镜像目录之外的路径必须被忽略（不得越界删除）")
}
