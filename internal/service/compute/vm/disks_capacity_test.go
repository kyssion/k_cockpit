package vm

import (
	"testing"

	"k_cockpit/internal/agent"
)

// 磁盘容量展示的回归：小于 1 GB 的盘不能显示成 0。
//
// 实机缺陷：节点按 `int(capacity/GiB)` 取整，一块 404 MB 的系统盘
// capacity_gb 变成 0，磁盘页签显示「0 GB」而盘实际可用——用户会以为
// 磁盘坏了。现在节点额外上报精确字节，控制面按两位小数折算。
func TestCapacityGBOfKeepsSubGigabyteDisks(t *testing.T) {
	cases := []struct {
		name      string
		disk      agent.VMDisk
		wantGB    float64
		wantBytes int64
	}{
		{
			name:      "精确字节优先：153 MB 的盘显示 0.14 GB 而不是 0",
			disk:      agent.VMDisk{CapacityGB: 0, CapacityBytes: 153047040},
			wantGB:    0.14,
			wantBytes: 153047040,
		},
		{
			name:      "整 GB 不受影响",
			disk:      agent.VMDisk{CapacityGB: 20, CapacityBytes: 20 << 30},
			wantGB:    20,
			wantBytes: 20 << 30,
		},
		{
			name:      "老节点只上报取整 GB 时按 GB 换算",
			disk:      agent.VMDisk{CapacityGB: 5},
			wantGB:    5,
			wantBytes: 5 << 30,
		},
		{
			name:   "两者都没有时保持 0（未知不是错误）",
			disk:   agent.VMDisk{},
			wantGB: 0, wantBytes: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.disk
			if got := capacityBytesOf(&d); got != tc.wantBytes {
				t.Errorf("capacityBytesOf = %d, 期望 %d", got, tc.wantBytes)
			}
			if got := capacityGBOf(&d); got != tc.wantGB {
				t.Errorf("capacityGBOf = %v, 期望 %v", got, tc.wantGB)
			}
		})
	}
}
