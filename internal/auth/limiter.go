package auth

import (
	"sync"
	"time"
)

// LimiterConfig 是登录限流的可调参数。
type LimiterConfig struct {
	// Window 是 IP 维度失败计数的统计窗口。
	Window time.Duration
	// IPLimit 是窗口内允许的失败次数，超过即拒绝。
	IPLimit int
	// AccountLimit 是账号维度触发延迟的失败次数。
	AccountLimit int
	// MaxDelay 是账号维度施加的最大延迟。
	MaxDelay time.Duration
	// MaxEntries 是记录上限，超过时触发一次过期清理，防止被大量伪造 IP 撑爆内存。
	MaxEntries int
}

// DefaultLimiterConfig 返回默认参数。
//
// 数值来源：IP 维度取「15 分钟内 20 次」——足以容纳正常用户连续输错，
// 又能有效压制单点爆破；账号维度采用**递增延迟而非硬锁**（f-1-01 Q-005），
// 避免攻击者用错误密码恶意锁定他人账号。
func DefaultLimiterConfig() LimiterConfig {
	return LimiterConfig{
		Window:       15 * time.Minute,
		IPLimit:      20,
		AccountLimit: 3,
		MaxDelay:     3 * time.Second,
		MaxEntries:   10000,
	}
}

// Limiter 是登录尝试的内存限流器（f-1-01 R-003 / Q-005）。
//
// 双维度计数：
//   - **按 IP**：超过阈值直接拒绝，防单点爆破；
//   - **按账号**：施加递增延迟，不锁定账号——否则攻击者可用错误密码
//     把任意用户锁在门外。
//
// 状态只存内存：进程重启后计数清零，这在单机面板场景下可接受
// （重启本身已是异常事件，且不会因此永久放行）。
type Limiter struct {
	mu      sync.Mutex
	records map[string]*attemptRecord
	cfg     LimiterConfig
	now     func() time.Time // 便于测试注入
}

// attemptRecord 是单个键（IP 或账号）的失败记录。
type attemptRecord struct {
	count int
	last  time.Time
}

// NewLimiter 构造限流器；cfg 中的零值会被默认值补齐。
func NewLimiter(cfg LimiterConfig) *Limiter {
	def := DefaultLimiterConfig()
	if cfg.Window <= 0 {
		cfg.Window = def.Window
	}
	if cfg.IPLimit <= 0 {
		cfg.IPLimit = def.IPLimit
	}
	if cfg.AccountLimit <= 0 {
		cfg.AccountLimit = def.AccountLimit
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = def.MaxDelay
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = def.MaxEntries
	}
	return &Limiter{
		records: make(map[string]*attemptRecord),
		cfg:     cfg,
		now:     time.Now,
	}
}

// Allow 报告某次登录尝试是否放行。
//
// 返回 false 时同时给出建议的重试等待时间，供 429 响应使用。
func (l *Limiter) Allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	rec := l.peek(ipKey(ip))
	if rec == nil || rec.count < l.cfg.IPLimit {
		return true, 0
	}
	return false, time.Until(rec.last.Add(l.cfg.Window)).Round(time.Second)
}

// Success 在登录成功后清除账号维度的失败计数。
//
// **不清除 IP 计数**：同一 IP 下的其他账号失败仍应计入，否则攻击者
// 只要用一个已知正确账号登录一次就能重置自己的爆破计数。
func (l *Limiter) Success(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.records, acctKey(username))
}

// Failure 记录一次登录失败，同时计入 IP 与账号两个维度。
func (l *Limiter) Failure(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.records) > l.cfg.MaxEntries {
		l.evictExpiredLocked()
	}
	l.bump(ipKey(ip))
	if username != "" {
		l.bump(acctKey(username))
	}
}

// Delay 返回账号维度当前应施加的延迟。
//
// 随失败次数递增，达到 MaxDelay 后不再增长。它只**拖慢**尝试速度，
// 不会阻止正确密码登录——这正是与「锁定账号」的关键差别。
func (l *Limiter) Delay(username string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	rec := l.peek(acctKey(username))
	if rec == nil || rec.count < l.cfg.AccountLimit {
		return 0
	}

	over := rec.count - l.cfg.AccountLimit + 1
	d := time.Duration(over) * 200 * time.Millisecond
	if d > l.cfg.MaxDelay {
		d = l.cfg.MaxDelay
	}
	return d
}

// peek 返回未过期的记录；已过期则顺手删除并返回 nil。调用方需持有锁。
func (l *Limiter) peek(key string) *attemptRecord {
	rec, ok := l.records[key]
	if !ok {
		return nil
	}
	if l.now().Sub(rec.last) > l.cfg.Window {
		delete(l.records, key)
		return nil
	}
	return rec
}

// bump 增加计数。调用方需持有锁。
func (l *Limiter) bump(key string) {
	rec := l.peek(key)
	if rec == nil {
		l.records[key] = &attemptRecord{count: 1, last: l.now()}
		return
	}
	rec.count++
	rec.last = l.now()
}

// evictExpiredLocked 清理过期记录。调用方需持有锁。
func (l *Limiter) evictExpiredLocked() {
	cutoff := l.now().Add(-l.cfg.Window)
	for k, rec := range l.records {
		if rec.last.Before(cutoff) {
			delete(l.records, k)
		}
	}
}

func ipKey(ip string) string  { return "ip:" + ip }
func acctKey(u string) string { return "acct:" + u }
