package hub

import (
	"encoding/json"
	"testing"

	"k_cockpit/internal/agent"
)

// TestDecodeVMDiskResizeInfo 覆盖第 2 轮 NEW-001 的解码器登记。
//
// 执行器要拿节点回传的**实测**容量才能把投影写成真实值。解码器缺席时断言
// 失败，执行器只能退回请求值——而请求值在并发扩容下恰恰是最不可信的那个。
// 因此这里逐字段核对，而不是只判类型：Applied / NewGB 任一丢失都会让写回
// 做出错误判断（Applied 丢失 → 被当成"没扩"，NewGB 丢失 → 被当成"没报容量"）。
func TestDecodeVMDiskResizeInfo(t *testing.T) {
	want := agent.VMDiskResizeInfo{
		Applied: true, OldGB: 100, NewGB: 220,
		GuestGrowNeeded: true,
		GuestGrowHint:   "growpart /dev/vda 1",
		Message:         "宿主机侧已扩容",
	}
	blob, err := json.Marshal(map[string]any{agent.VMDiskDataKey: want})
	if err != nil {
		t.Fatalf("编码节点结果失败: %v", err)
	}

	out, err := decodeResultData(agent.OpVMDiskResize, string(blob))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	got, ok := out[agent.VMDiskDataKey].(agent.VMDiskResizeInfo)
	if !ok {
		t.Fatalf("vm_disk 应还原为 VMDiskResizeInfo，实际 %T", out[agent.VMDiskDataKey])
	}
	if got != want {
		t.Errorf("扩容结果还原不对: %+v，期望 %+v", got, want)
	}
}

// TestDecodeVMDiskResizeNotAppliedKept 证明 Applied=false 不会被解码吞掉。
//
// 这是最容易写错的一种：如果解码器只解出 NewGB，一个 Applied=false 的结果
// 会看起来像"扩了但没报容量"，进而被当成另一种失败原因；而两者对用户说的是
// 完全不同的两件事。
func TestDecodeVMDiskResizeNotAppliedKept(t *testing.T) {
	blob, err := json.Marshal(map[string]any{
		agent.VMDiskDataKey: agent.VMDiskResizeInfo{Applied: false, OldGB: 100},
	})
	if err != nil {
		t.Fatalf("编码节点结果失败: %v", err)
	}

	out, err := decodeResultData(agent.OpVMDiskResize, string(blob))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	got, ok := out[agent.VMDiskDataKey].(agent.VMDiskResizeInfo)
	if !ok {
		t.Fatalf("vm_disk 应还原为 VMDiskResizeInfo，实际 %T", out[agent.VMDiskDataKey])
	}
	if got.Applied {
		t.Error("Applied 应为 false，不能被解码吞成 true")
	}
	if got.NewGB != 0 {
		t.Errorf("NewGB 应为 0，实际 %d", got.NewGB)
	}
}
