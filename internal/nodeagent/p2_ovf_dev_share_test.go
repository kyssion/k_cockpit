package nodeagent

import (
	"encoding/xml"
	"strings"
	"testing"
)

// 本文件覆盖第 3 轮三条 P2：
//   - DEF-R3-07：OVF 的 capacity 没按 capacityAllocationUnits 换算；
//   - DEF-R3-08：请求的设备名被静默改写（前缀与总线不匹配）；
//   - DEF-R3-09：卸载不存在的共享返回 success。

// TestOVFCapacityBytes 覆盖单位换算（DEF-R3-07 的根因）。
func TestOVFCapacityBytes(t *testing.T) {
	cases := []struct {
		capacity int64
		units    string
		want     int64
	}{
		{8589934592, "", 8589934592},      // 缺省 = 字节
		{8, "byte * 2^30", 8 << 30},       // 真机那条：8 GiB
		{8, "byte*2^30", 8 << 30},         // 无空格写法
		{2048, "byte * 2^20", 2048 << 20}, // MiB
		{8, "gigabyte", 8 << 30},          // 词形写法
		{1, "terabyte", 1 << 40},          // TB
		{8, "BYTE * 2^30", 8 << 30},       // 大小写不敏感
		{0, "byte * 2^30", 0},             // 0 就是 0
		{-1, "byte * 2^30", 0},            // 负数按 0 处理（由兜底逻辑接手）
		{5, "furlong", 5},                 // 不认识 → 按字节，不放大
	}
	for _, tc := range cases {
		if got := ovfCapacityBytes(tc.capacity, tc.units); got != tc.want {
			t.Errorf("capacity=%d units=%q → %d，期望 %d", tc.capacity, tc.units, got, tc.want)
		}
	}
}

// TestOVFParseDiskCapacity 覆盖"8 GB 的 OVF 必须解析成 8"。
//
// 两种写法都要成立：字节写法（8589934592）与规范里的单位写法（8 + byte*2^30）。
// 真机当初就是后者被当成 8 **字节**，向上取整成 1 GB。
func TestOVFParseDiskCapacity(t *testing.T) {
	docs := map[string]string{
		"字节写法": `<Envelope><DiskSection><Disk capacity="8589934592" fileRef="d.vmdk"/></DiskSection></Envelope>`,
		"单位写法": `<Envelope xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"><ovf:DiskSection><ovf:Disk ovf:capacity="8" ovf:capacityAllocationUnits="byte * 2^30" ovf:fileRef="d.vmdk"/></ovf:DiskSection></Envelope>`,
		// DiskSection 放在 VirtualSystem 之后也要能解析（顺序不是根因，钉住这一点）
		"顺序颠倒": `<Envelope><VirtualSystem><VirtualHardwareSection/></VirtualSystem><DiskSection><Disk capacity="8" capacityAllocationUnits="byte * 2^30"/></DiskSection></Envelope>`,
	}
	for name, doc := range docs {
		var env ovfEnvelope
		if err := xml.Unmarshal([]byte(doc), &env); err != nil {
			t.Fatalf("%s: 解析失败: %v", name, err)
		}
		var capacity int64
		for _, d := range env.DiskSection.Disks {
			if b := ovfCapacityBytes(d.Capacity, d.CapacityAllocationUnits); b > capacity {
				capacity = b
			}
		}
		if got := int((capacity + (1 << 30) - 1) >> 30); got != 8 {
			t.Errorf("%s: disk_gb = %d，期望 8", name, got)
		}
	}
}

// TestDevPrefixForBus 覆盖设备名前缀与总线的对应（DEF-R3-08）。
//
// 前缀不匹配时必须给出**明确错误**，而不是把 vdb 悄悄改成 sdb。
func TestDevPrefixForBus(t *testing.T) {
	for bus, want := range map[string]string{
		"virtio": "vd", "scsi": "sd", "sata": "sd", "ide": "hd", "usb": "",
	} {
		if got := devPrefixForBus(bus); got != want {
			t.Errorf("bus=%s → %q，期望 %q", bus, got, want)
		}
	}
	// 真机那条：请求 vdb、总线被降级到 scsi → 前缀 sd ≠ vd，必须拒绝而不是改写。
	if strings.HasPrefix("vdb", devPrefixForBus("scsi")) {
		t.Error("vdb 在 scsi 总线上不该被认为合法（这正是被静默改写成 sdb 的那条）")
	}
}

// TestShareTagExists 覆盖卸载前的存在性判断（DEF-R3-09）。
func TestShareTagExists(t *testing.T) {
	doc := `<domain><devices>
    <filesystem type='mount' accessmode='passthrough'>
      <driver type='path'/>
      <source dir='/var/lib/k_cockpit/userdata/share'/>
      <target dir='kcshare'/>
    </filesystem>
  </devices></domain>`
	if !shareTagExists(doc, "kcshare") {
		t.Error("存在的共享应被识别")
	}
	for _, absent := range []string{"other", "kc", ""} {
		if absent == "" {
			continue
		}
		if shareTagExists(doc, absent) {
			t.Errorf("%q 不存在，不该被判为存在", absent)
		}
	}
	if shareTagExists("<domain><devices></devices></domain>", "x") {
		t.Error("没有任何 filesystem 时不该判为存在")
	}
}
