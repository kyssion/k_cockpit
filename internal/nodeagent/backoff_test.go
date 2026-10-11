package nodeagent

import (
	"testing"
	"time"
)

// TestNextBackoffGrowth 验证退避按 1s ×1.6 增长并在 60s 封顶（f-6-01 §5.2）。
func TestNextBackoffGrowth(t *testing.T) {
	cases := []struct {
		failures int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{1, 800 * time.Millisecond, 1200 * time.Millisecond},  // 1s ±20%
		{2, 1280 * time.Millisecond, 1920 * time.Millisecond}, // 1.6s ±20%
		{5, 5 * time.Second, 10 * time.Second},                // ~6.5s ±20%
		{30, 48 * time.Second, 72 * time.Second},              // 封顶 60s ±20%
	}
	for _, c := range cases {
		got := nextBackoff(c.failures, 0)
		if got < c.wantMin || got > c.wantMax {
			t.Errorf("failures=%d: got %s，期望在 [%s, %s]", c.failures, got, c.wantMin, c.wantMax)
		}
	}
}

// TestNextBackoffResetsAfterStableSession 稳定会话（≥30s）之后失败计数归零：
// 长连接证明对端是活的，断开后应按首次节奏立即重试，而不是继续翻倍等待。
func TestNextBackoffResetsAfterStableSession(t *testing.T) {
	got := nextBackoff(20, stableAfter)
	if got > 1200*time.Millisecond {
		t.Errorf("稳定会话后退避应回到首次节奏，实际 %s", got)
	}
}

// TestJitterBounds 抖动不应把等待时间变成 0 或负值。
func TestJitterBounds(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := jitter(time.Second)
		if got < 800*time.Millisecond || got > 1200*time.Millisecond {
			t.Fatalf("抖动越界: %s", got)
		}
	}
}
