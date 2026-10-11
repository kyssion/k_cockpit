package nodeagent

import (
	"math/rand"
	"time"
)

// 重连退避参数（f-6-01 §5.2：1s 起、×1.6、上限 60s、±20% 抖动）。
const (
	backoffBase   = 1 * time.Second
	backoffFactor = 1.6
	backoffCap    = 60 * time.Second
	jitterRatio   = 0.2
)

// stableAfter 是「算一次稳定会话」的存活时长：达到它之后失败计数归零，
// 断开后按首次重连的节奏尝试。退避表达的是「对端可能挂了，别敲门太勤」，
// 而一段持续了半分钟的连接足以证明对端是活的。
const stableAfter = 30 * time.Second

// nextBackoff 由连续失败次数计算下一次重连等待。
func nextBackoff(failures int, lastSession time.Duration) time.Duration {
	if lastSession >= stableAfter {
		failures = 0
	}
	if failures <= 0 {
		failures = 1
	}
	d := backoffBase
	for i := 1; i < failures; i++ {
		d = time.Duration(float64(d) * backoffFactor)
		if d >= backoffCap {
			return jitter(backoffCap)
		}
	}
	return jitter(d)
}

// jitter 给等待时间加 ±ratio 的随机抖动，避免多台 agent 断网恢复后
// 在同一毫秒齐步重连。
func jitter(d time.Duration) time.Duration {
	delta := float64(d) * jitterRatio
	return time.Duration(float64(d) + (rand.Float64()*2-1)*delta)
}
