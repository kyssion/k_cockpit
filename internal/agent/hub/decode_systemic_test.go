package hub

import (
	"encoding/json"
	"reflect"
	"testing"

	"k_cockpit/internal/agent"
)

// TestDecodeVMXMLValidateInfo 覆盖 DEF-001 的根因：节点回传的 vm_xml_validate
// 必须还原成 agent.VMXMLValidateInfo，且 Valid / Errors / Warnings 三个字段
// 都要逐个对上——只判类型是弱断言，字段丢失同样会让 precheck 静默失败。
func TestDecodeVMXMLValidateInfo(t *testing.T) {
	want := agent.VMXMLValidateInfo{
		Valid:    true,
		Errors:   []string{"第 3 行：unknown element <bogus/>", "第 9 行：missing <memory/>"},
		Warnings: []string{"下次重启后才会生效"},
	}
	blob, err := json.Marshal(map[string]any{agent.VMXMLValidateKey: want})
	if err != nil {
		t.Fatalf("编码节点结果失败: %v", err)
	}

	out, err := decodeResultData(agent.OpVMXMLApply, string(blob))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	got, ok := out[agent.VMXMLValidateKey].(agent.VMXMLValidateInfo)
	if !ok {
		t.Fatalf("vm_xml_validate 应还原为 VMXMLValidateInfo，实际 %T", out[agent.VMXMLValidateKey])
	}
	if got.Valid != want.Valid {
		t.Errorf("Valid = %v, 期望 %v", got.Valid, want.Valid)
	}
	if !reflect.DeepEqual(got.Errors, want.Errors) {
		t.Errorf("Errors = %v, 期望 %v", got.Errors, want.Errors)
	}
	if !reflect.DeepEqual(got.Warnings, want.Warnings) {
		t.Errorf("Warnings = %v, 期望 %v", got.Warnings, want.Warnings)
	}
}

// TestDecodeVMXMLApplyDefineUnaffected 证明 define 路径不受新增解码器影响：
// 它的 Data 为空（"" / "{}"），decodeResultData 直接返回 nil，不会凭空造出
// 一个零值 VMXMLValidateInfo，也不会报错。
func TestDecodeVMXMLApplyDefineUnaffected(t *testing.T) {
	for _, raw := range []string{"", "{}"} {
		out, err := decodeResultData(agent.OpVMXMLApply, raw)
		if err != nil || out != nil {
			t.Errorf("define Data=%q 应返回 nil,nil，实际 %v,%v", raw, out, err)
		}
	}
}

// TestDecodeSystemicRegistrations 覆盖系统性排查补齐的解码器：每一条都是
// 「节点返回结构体/切片 → 服务层用类型断言消费」的 (OpKind, key) 组合。
// 断言还原后的具体类型，并抽查代表字段，防止登记了键却写错目标类型。
func TestDecodeSystemicRegistrations(t *testing.T) {
	cases := []struct {
		name string
		kind agent.OpKind
		key  string
		val  any
		want any // 期望还原出的值；用 reflect.DeepEqual 比对
	}{
		{
			name: "image.import", kind: agent.OpImageImport, key: agent.ImageImportDataKey,
			val:  agent.ImageImportInfo{DiskPath: "/images/a.qcow2", SizeGB: 8, Format: "qcow2", Notes: []string{"vmdk 转换"}},
			want: agent.ImageImportInfo{DiskPath: "/images/a.qcow2", SizeGB: 8, Format: "qcow2", Notes: []string{"vmdk 转换"}},
		},
		{
			name: "image.parse", kind: agent.OpImageParse, key: agent.ImageParseDataKey,
			val:  agent.ImagePreview{VCPU: 4, MemoryMB: 8192, DiskGB: 40, OSType: "linux", OSVariant: "ubuntu24.04", Sources: map[string]string{"vcpu": "ovf"}},
			want: agent.ImagePreview{VCPU: 4, MemoryMB: 8192, DiskGB: 40, OSType: "linux", OSVariant: "ubuntu24.04", Sources: map[string]string{"vcpu": "ovf"}},
		},
		{
			name: "template.prepare", kind: agent.OpTemplatePrepare, key: agent.TemplateDataKey,
			val:  agent.TemplateInfo{DiskPath: "/templates/t.qcow2", SizeGB: 20, Format: "qcow2"},
			want: agent.TemplateInfo{DiskPath: "/templates/t.qcow2", SizeGB: 20, Format: "qcow2"},
		},
		{
			name: "vm.adopt", kind: agent.OpVMAdopt, key: agent.VMAdoptDataKey,
			val:  agent.VMAdoptInfo{UUID: "11111111-2222-3333-4444-555555555555"},
			want: agent.VMAdoptInfo{UUID: "11111111-2222-3333-4444-555555555555"},
		},
		{
			name: "vm.guest", kind: agent.OpVMGuest, key: agent.GuestDataKey,
			val:  agent.GuestInfo{Message: "已在线修改 root 密码", GuestAgentUsed: true},
			want: agent.GuestInfo{Message: "已在线修改 root 密码", GuestAgentUsed: true},
		},
		{
			name: "network.capture.fetch", kind: agent.OpCaptureFetch, key: agent.CaptureContentKey,
			val:  agent.ExportContent{Data: []byte("pcap-bytes"), MIME: "application/vnd.tcpdump.pcap"},
			want: agent.ExportContent{Data: []byte("pcap-bytes"), MIME: "application/vnd.tcpdump.pcap"},
		},
		{
			name: "port_security.precheck", kind: agent.OpPortSecurityPrecheck, key: agent.PortSecurityPrecheckKey,
			val:  agent.PortSecurityPrecheck{Rules: []string{"flow-1"}, Warnings: []string{"已有手工流表"}},
			want: agent.PortSecurityPrecheck{Rules: []string{"flow-1"}, Warnings: []string{"已有手工流表"}},
		},
		{
			name: "public.ip.preview", kind: agent.OpPublicIPPreview, key: agent.PublicIPPreviewKey,
			val:  agent.PublicIPPreview{Added: []string{"iptables -t nat -A ..."}, Warnings: []string{"地址已被占用"}},
			want: agent.PublicIPPreview{Added: []string{"iptables -t nat -A ..."}, Warnings: []string{"地址已被占用"}},
		},
		{
			name: "storage.pool.scan", kind: agent.OpStoragePoolScan, key: agent.VolumeListKey,
			val:  []string{"vol-a.qcow2", "vol-b.qcow2"},
			want: []string{"vol-a.qcow2", "vol-b.qcow2"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob, err := json.Marshal(map[string]any{tc.key: tc.val})
			if err != nil {
				t.Fatalf("编码节点结果失败: %v", err)
			}
			out, err := decodeResultData(tc.kind, string(blob))
			if err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			got := out[tc.key]
			if reflect.TypeOf(got) != reflect.TypeOf(tc.want) {
				t.Fatalf("%s 应还原为 %T，实际 %T", tc.key, tc.want, got)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("字段不符:\n got = %+v\nwant = %+v", got, tc.want)
			}
		})
	}
}

// TestDecodeVolumeListSlice 专门覆盖 OpStoragePoolScan 的 []string：切片不是
// 结构体，into([]string{}) 走 reflect.New(reflect.TypeOf(sample)) 得到
// *[]string，json.Unmarshal 后 Elem().Interface() 还原为 []string。这里显式
// 断言元素个数与逐个元素的具体值（不只判类型），并用一个未登记的 Kind 作
// 反证：通用 JSON 解码会把 JSON 数组解成 []any，服务层的 .([]string) 必然
// 失败——这正是「漏登记导致占用检查恒判池内无卷」的原因。
func TestDecodeVolumeListSlice(t *testing.T) {
	want := []string{"vol-a.qcow2", "vol-b.qcow2", "vol-c.raw"}
	blob, err := json.Marshal(map[string]any{agent.VolumeListKey: want})
	if err != nil {
		t.Fatalf("编码节点结果失败: %v", err)
	}

	out, err := decodeResultData(agent.OpStoragePoolScan, string(blob))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	got, ok := out[agent.VolumeListKey].([]string)
	if !ok {
		t.Fatalf("volumes 应还原为 []string，实际 %T", out[agent.VolumeListKey])
	}
	if len(got) != len(want) {
		t.Fatalf("元素个数 = %d，期望 %d（切片不能退化为空）", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}

	// 反证：未登记的 Kind 走通用解码，数组变成 []any，. ([]string) 失败。
	genericOut, err := decodeResultData(agent.OpKind("storage.pool.unregistered"), string(blob))
	if err != nil {
		t.Fatalf("通用解码失败: %v", err)
	}
	if _, ok := genericOut[agent.VolumeListKey].([]string); ok {
		t.Error("未登记 Kind 不应还原为 []string（否则漏登记不会造成静默失败，论证不成立）")
	}
	if anySlice, ok := genericOut[agent.VolumeListKey].([]any); !ok || len(anySlice) != len(want) {
		t.Errorf("未登记 Kind 应退化为 []any，实际 %T", genericOut[agent.VolumeListKey])
	}
}
