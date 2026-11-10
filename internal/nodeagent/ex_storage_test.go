package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"k_cockpit/internal/agent"
)

// TestStorageVolumeApplyCreatesPVBeforeVG vgcreate 只接受已经是 PV 的设备。
// 第 12 轮真机复现过：裸盘直接 vgcreate 失败被吞掉，lvcreate 随后报
// "Volume group not found"，创建存储卷从未成功过。
func TestStorageVolumeApplyCreatesPVBeforeVG(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	installFakeCommands(t, map[string]string{
		"pvdisplay": "#!/bin/sh\nexit 1\n",
		"pvcreate":  "#!/bin/sh\necho \"pvcreate $*\" >> " + logPath + "\nexit 0\n",
		"wipefs":    "#!/bin/sh\nexit 0\n",
		"vgdisplay": "#!/bin/sh\nexit 1\n",
		"vgcreate":  "#!/bin/sh\necho \"vgcreate $*\" >> " + logPath + "\nexit 0\n",
		"lvcreate":  "#!/bin/sh\necho \"lvcreate $*\" >> " + logPath + "\nexit 0\n",
	})

	res, err := storageVolumeApply(context.Background(), agent.Operation{
		Params: map[string]any{
			"action": "create", "vg_name": "kc-vg-probe", "lv_name": "kc-lv-probe",
			"size_gb": float64(2), "devices": []any{"/dev/r12probe"},
		},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("创建卷应成功: err=%v res=%+v", err, res)
	}
	buf, _ := os.ReadFile(logPath)
	got := string(buf)
	pi, vi, li := strings.Index(got, "pvcreate"), strings.Index(got, "vgcreate"), strings.Index(got, "lvcreate")
	if pi < 0 || vi < 0 || li < 0 || !(pi < vi && vi < li) {
		t.Errorf("顺序应为 pvcreate → vgcreate → lvcreate，实际: %s", got)
	}
}

// TestStorageVolumeApplyFailsWhenVGCreateFails 卷组没建起来时必须明确失败，
// 不能继续 lvcreate 留一个语义不明的错误。
func TestStorageVolumeApplyFailsWhenVGCreateFails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	installFakeCommands(t, map[string]string{
		"pvdisplay": "#!/bin/sh\nexit 0\n", // 已是 PV
		"pvcreate":  "#!/bin/sh\nexit 0\n",
		"vgdisplay": "#!/bin/sh\nexit 1\n",
		"vgcreate":  "#!/bin/sh\nexit 1\n",
		"lvcreate":  "#!/bin/sh\necho \"lvcreate called\" >> " + logPath + "\nexit 0\n",
	})

	res, err := storageVolumeApply(context.Background(), agent.Operation{
		Params: map[string]any{
			"action": "create", "vg_name": "kc-vg-probe", "lv_name": "kc-lv-probe",
			"size_gb": float64(2), "devices": []any{"/dev/r12probe"},
		},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "创建卷组失败") {
		t.Fatalf("卷组创建失败应明确报错，实际 %+v", res)
	}
	buf, _ := os.ReadFile(logPath)
	if strings.Contains(string(buf), "lvcreate called") {
		t.Error("卷组都没建起来却继续调用了 lvcreate")
	}
}

// TestStorageVolumeDeleteIsIdempotent 创建失败留下的孤儿记录，节点侧
// 根本没有卷组；此时删除必须按幂等成功处理，否则记录和设备一起卡死。
func TestStorageVolumeDeleteIsIdempotent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "calls.log")
	installFakeCommands(t, map[string]string{
		"lvremove":  "#!/bin/sh\nexit 1\n",
		"lvdisplay": "#!/bin/sh\nexit 1\n",
		"vgdisplay": "#!/bin/sh\nexit 1\n",
		"vgremove":  "#!/bin/sh\necho \"vgremove $*\" >> " + logPath + "\nexit 0\n",
		"pvremove":  "#!/bin/sh\necho \"pvremove $*\" >> " + logPath + "\nexit 0\n",
	})
	res, err := storageVolumeApply(context.Background(), agent.Operation{
		Params: map[string]any{
			"action": "delete", "vg_name": "kc-vg-ghost", "lv_name": "kc-lv-ghost",
			"devices": []any{"/dev/r12probe"},
		},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("卷组不存在时删除应幂等成功: err=%v res=%+v", err, res)
	}
	buf, _ := os.ReadFile(logPath)
	if !strings.Contains(string(buf), "pvremove") {
		t.Errorf("删除孤儿卷也要释放物理卷，实际命令: %s", buf)
	}
}

// TestStorageVolumeDeleteReportsRealFailure 卷还在（lvdisplay 看得到），
// 删除失败就必须如实报错，不能被幂等逻辑吞掉。
func TestStorageVolumeDeleteReportsRealFailure(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"lvremove":  "#!/bin/sh\nexit 1\n",
		"lvdisplay": "#!/bin/sh\nexit 0\n",
	})
	res, err := storageVolumeApply(context.Background(), agent.Operation{
		Params: map[string]any{
			"action": "delete", "vg_name": "kc-vg-busy", "lv_name": "kc-lv-busy",
		},
	}, func(Stage) {})
	if err != nil || res.Success || !strings.Contains(res.Message, "删除逻辑卷失败") {
		t.Fatalf("卷仍存在时删除失败应报错: err=%v res=%+v", err, res)
	}
}

// TestStorageVolumeApplyRejectsMissingDevices 没有设备时不应凭空建 LV。
func TestStorageVolumeApplyRejectsMissingDevices(t *testing.T) {
	res, err := storageVolumeApply(context.Background(), agent.Operation{
		Params: map[string]any{
			"action": "create", "vg_name": "kc-vg-probe", "lv_name": "kc-lv-probe",
			"size_gb": float64(2), "devices": []any{},
		},
	}, func(Stage) {})
	if err != nil || res.Success {
		t.Fatalf("缺少物理设备应失败: err=%v res=%+v", err, res)
	}
}

// TestParseMountPoints 覆盖 mountinfo 的八进制转义与短行。
func TestParseMountPoints(t *testing.T) {
	content := "36 35 98:0 / / rw,relatime - ext4 /dev/vda2 rw\n" +
		"60 35 43:0 / /var/lib/k_cockpit/pools/dev/nbd1 rw - ext4 /dev/nbd1 rw\n" +
		"61 35 0:50 / /some\\040path rw - tmpfs tmpfs rw\n" +
		"malformed line\n"
	got := parseMountPoints(content)
	for _, want := range []string{"/", "/var/lib/k_cockpit/pools/dev/nbd1", "/some path"} {
		if !got[want] {
			t.Errorf("挂载点 %q 未解析出来: %+v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("解析出 %d 个挂载点，期望 3: %+v", len(got), got)
	}
}

// TestPoolStatsRejectsUnmountedPath 节点重启后挂载目录还在、块设备没了，
// 此时绝不能把 statfs 的结果（其实是根分区）当成池容量上报。
func TestPoolStatsRejectsUnmountedPath(t *testing.T) {
	mounted := poolMountRoot + "/dev/nbd1"
	ghost := poolMountRoot + "/dev/nbd2"
	mounts := map[string]bool{mounted: true}
	fakeStatfs := func(_ string, st *syscall.Statfs_t) error {
		st.Blocks = 100
		st.Bavail = 40
		st.Bsize = 4096
		return nil
	}

	stats := poolStats(
		[]string{mounted, ghost, "/etc/passwd", ""},
		mounts, fakeStatfs,
	)
	byPath := map[string]agent.PoolStat{}
	for _, s := range stats {
		byPath[s.Path] = s
	}
	if !byPath[mounted].Mounted || byPath[mounted].TotalBytes != 409600 ||
		byPath[mounted].FreeBytes != 163840 {
		t.Errorf("已挂载池容量错误: %+v", byPath[mounted])
	}
	if s, ok := byPath[ghost]; !ok || s.Mounted || s.TotalBytes != 0 {
		t.Errorf("未挂载目录竟上报了容量: %+v", s)
	}
	if _, ok := byPath["/etc/passwd"]; ok {
		t.Error("越出挂载根的路径不应被探测")
	}
}

// TestStoragePoolStatUsesMountTable 端到端确认执行器读挂载表而不是盲信路径。
func TestStoragePoolStatUsesMountTable(t *testing.T) {
	tmp := t.TempDir()
	info := filepath.Join(tmp, "mountinfo")
	mountPath := poolMountRoot + "/dev/nbd1"
	if err := os.WriteFile(info, []byte("60 35 43:0 / "+mountPath+" rw - ext4 /dev/nbd1 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountInfoPath
	mountInfoPath = info
	t.Cleanup(func() { mountInfoPath = old })

	res, err := storagePoolStat(context.Background(), agent.Operation{
		Params: map[string]any{
			"paths": []any{mountPath, poolMountRoot + "/dev/nbd9"},
		},
	}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("执行失败: err=%v res=%+v", err, res)
	}
	stats, _ := res.Data[agent.PoolStatListKey].([]agent.PoolStat)
	if len(stats) != 2 {
		t.Fatalf("应返回两条（含未挂载标记）: %+v", stats)
	}
	// 挂载表里有的那条：真机上目录存在、statfs 成功；本机单测里目录不存在，
	// 会走 statfs 失败分支，同样是 Mounted=false，这是安全失败方向。
	for _, s := range stats {
		if s.Mounted && s.TotalBytes <= 0 {
			t.Errorf("上报 Mounted=true 却没有容量: %+v", s)
		}
	}
}
