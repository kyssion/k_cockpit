package nodeagent

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

const testLSBLK = `{"blockdevices":[
 {"name":"sdb","path":"/dev/sdb","size":107374182400,"type":"disk","fstype":null,"mountpoint":null,
  "children":[
   {"name":"sdb1","path":"/dev/sdb1","size":536870912,"type":"part","fstype":"vfat","mountpoint":"/boot/efi"},
   {"name":"sdb2","path":"/dev/sdb2","size":107373649920,"type":"part","fstype":"ext4","mountpoint":"/var/lib/k_cockpit/pools/sdb2"}
  ]}
]}`

func TestStoragePartitions(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"lsblk": "#!/bin/sh\ncat <<'EOF'\n" + testLSBLK + "\nEOF\n",
	})
	res, err := vmStoragePartitions(context.Background(), agent.Operation{Target: "/dev/sdb"}, func(Stage) {})
	if err != nil || !res.Success {
		t.Fatalf("分区列表应成功: err=%v res=%+v", err, res)
	}
	rows, ok := res.Data[agent.PartitionListDataKey].([]agent.PartitionInfo)
	if !ok || len(rows) != 2 {
		t.Fatalf("分区数量不对: %#v", res.Data)
	}
	if rows[0].Index != 1 || !rows[0].System || rows[0].FSType != "vfat" {
		t.Errorf("系统分区解析不对: %+v", rows[0])
	}
	if rows[1].InUseByPool != "sdb2" || !rows[1].Mounted {
		t.Errorf("存储池分区解析不对: %+v", rows[1])
	}
}

func TestStoragePartitionsRejectsUnknownDevice(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"lsblk": "#!/bin/sh\necho '{\"blockdevices\":[]}'\n",
	})
	res, err := vmStoragePartitions(context.Background(), agent.Operation{Target: "/dev/sdz"}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "找不到设备") {
		t.Fatalf("未知设备应失败: %+v", res)
	}
}

func TestHostPCIBindRejectsBadAddress(t *testing.T) {
	res, err := vmHostPCIBind(context.Background(), agent.Operation{
		Params: map[string]any{"address": "../../etc", "action": "bind"},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "格式") {
		t.Fatalf("非法 PCI 地址应拒绝: %+v", res)
	}
}

func TestHostTuningRejectsUnknownItem(t *testing.T) {
	res, err := vmHostTuningApply(context.Background(), agent.Operation{
		Params: map[string]any{"item": "turbo", "enabled": true},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if res.Success || !strings.Contains(res.Message, "未知") {
		t.Fatalf("未知调优项应拒绝: %+v", res)
	}
}

func TestTrailingNumber(t *testing.T) {
	cases := map[string]int{"sdb1": 1, "nvme0n1p12": 12, "vda": 0}
	for name, want := range cases {
		if got := trailingNumber(name); got != want {
			t.Errorf("trailingNumber(%q)=%d want %d", name, got, want)
		}
	}
}

func TestParseFlexSize(t *testing.T) {
	cases := map[string]int64{
		"1024": 1024,
		"1G":   1 << 30,
		"1.5M": 1572864,
		"":     0,
		"bad":  0,
	}
	for in, want := range cases {
		if got := parseFlexSize(in); got != want {
			t.Errorf("parseFlexSize(%q)=%d want %d", in, got, want)
		}
	}
}
