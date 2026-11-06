package hosttuning

import (
	"testing"
)

// 本文件覆盖第 3 轮 DEF-R3-10：负数 disksize_mb 被受理。
//
// 旧判断是"大于 128 才算合理"的形态：`-1` 既不满足 `< 128` 也不满足 `> 1TB`，
// 一路入队；节点侧又用 `sizeMB > 0` 跳过设置 → 任务成功而 ZRAM 大小没变。
func TestValidateZRAMRejectsNegative(t *testing.T) {
	req := Request{Item: "zram", DisksizeMB: -1}
	if err := validateZRAM(req); err == nil {
		t.Fatal("负数容量必须被拒绝（否则任务成功但什么都没做）")
	}
	if err := validateZRAM(Request{Item: "zram", DisksizeMB: -1024}); err == nil {
		t.Fatal("负数容量必须被拒绝")
	}
}

// TestValidateZRAMBounds 覆盖其余边界不被这次改动弄坏。
func TestValidateZRAMBounds(t *testing.T) {
	ok := []int64{0, 128, 4096, 1024 * 1024}
	for _, v := range ok {
		if err := validateZRAM(Request{Item: "zram", DisksizeMB: v}); err != nil {
			t.Errorf("%d MB 应被接受，实际: %v", v, err)
		}
	}
	bad := []int64{1, 127, 1024*1024 + 1}
	for _, v := range bad {
		if err := validateZRAM(Request{Item: "zram", DisksizeMB: v}); err == nil {
			t.Errorf("%d MB 应被拒绝", v)
		}
	}
	if err := validateZRAM(Request{Item: "zram", DisksizeMB: 1024, Algorithm: "bogus"}); err == nil {
		t.Error("未知压缩算法应被拒绝")
	}
	if err := validateZRAM(Request{Item: "zram", DisksizeMB: 1024, MemLimitMB: -1}); err == nil {
		t.Error("负数内存阈值应被拒绝")
	}
}
