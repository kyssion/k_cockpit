package risk

import (
	"sync"
	"time"
)

// 验证尝试的限流参数。
const (
	// attemptWindow 是失败计数的统计窗口。
	attemptWindow = 15 * time.Minute
	// attemptLimit 是窗口内允许的失败次数，超过即拒绝。
	//
	// 取 5 而非更宽松的值：TOTP 只有 6 位数字，一个时间窗内 5 次机会已经
	// 足够覆盖手误，再放宽就接近「暴力枚举可行」的边界。
	attemptLimit = 5
	// maxAttemptRecords 是记录上限，超过时触发一次过期清理，
	// 防止被大量伪造账号撑爆内存。
	maxAttemptRecords = 10000
)

// attemptLimiter 按**用户**限制验证尝试次数（R-010）。
//
// 与登录限流是独立实例、独立计数：登录失败与验证失败是不同的攻击面，
// 共用计数会让用户在登录时输错几次之后，连带在二次验证里被限流——两件
// 无关的事绑在一起，用户只会觉得「系统莫名其妙不让我操作」。
type attemptLimiter struct {
	mu      sync.Mutex
	records map[int64]*attemptRecord
	now     func() time.Time // 便于测试注入
}

type attemptRecord struct {
	count int
	last  time.Time
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{
		records: make(map[int64]*attemptRecord),
		now:     time.Now,
	}
}

// allow 报告该用户是否还能继续尝试；不允许时给出可重试时间。
func (l *attemptLimiter) allow(userID int64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	rec, ok := l.records[userID]
	if !ok {
		return true, 0
	}
	if now.Sub(rec.last) > attemptWindow {
		// 窗口已过，计数自然失效。
		delete(l.records, userID)
		return true, 0
	}
	if rec.count < attemptLimit {
		return true, 0
	}
	return false, time.Until(rec.last.Add(attemptWindow)).Round(time.Second)
}

// failure 记一次失败。
func (l *attemptLimiter) failure(userID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.records) >= maxAttemptRecords {
		l.evictExpiredLocked(now)
	}

	rec, ok := l.records[userID]
	if !ok || now.Sub(rec.last) > attemptWindow {
		l.records[userID] = &attemptRecord{count: 1, last: now}
		return
	}
	rec.count++
	rec.last = now
}

// success 清空该用户的失败计数。
//
// 验证成功后清零：否则用户在长期使用中累积的偶发手误会在某天突然把他
// 锁在门外，而他完全不知道自己「什么时候犯过错」。
func (l *attemptLimiter) success(userID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.records, userID)
}

// remaining 返回窗口内剩余尝试次数。
func (l *attemptLimiter) remaining(userID int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.records[userID]
	if !ok || l.now().Sub(rec.last) > attemptWindow {
		return attemptLimit
	}
	if rec.count >= attemptLimit {
		return 0
	}
	return attemptLimit - rec.count
}

// evictExpiredLocked 清理过期记录。调用方须持有锁。
func (l *attemptLimiter) evictExpiredLocked(now time.Time) {
	for id, rec := range l.records {
		if now.Sub(rec.last) > attemptWindow {
			delete(l.records, id)
		}
	}
}
