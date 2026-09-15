package auth

import (
	"testing"
	"time"
)

// newTestLimiter 构造一个时间可控的限流器。
func newTestLimiter(cfg LimiterConfig) (*Limiter, *time.Time) {
	l := NewLimiter(cfg)
	now := time.Now()
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterAllowsUntilThreshold(t *testing.T) {
	l, _ := newTestLimiter(LimiterConfig{IPLimit: 3, Window: time.Minute})

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("1.2.3.4"); !ok {
			t.Fatalf("第 %d 次尝试不应被拒绝", i+1)
		}
		l.Failure("1.2.3.4", "alice")
	}

	if ok, retry := l.Allow("1.2.3.4"); ok {
		t.Error("超过阈值后仍被放行")
	} else if retry <= 0 {
		t.Errorf("应给出重试等待时间, 实际 %v", retry)
	}
}

// 按 IP 计数不能影响其他 IP。
func TestLimiterIsolatesIPs(t *testing.T) {
	l, _ := newTestLimiter(LimiterConfig{IPLimit: 2, Window: time.Minute})

	l.Failure("1.1.1.1", "alice")
	l.Failure("1.1.1.1", "alice")

	if ok, _ := l.Allow("1.1.1.1"); ok {
		t.Error("超限 IP 应被拒绝")
	}
	if ok, _ := l.Allow("2.2.2.2"); !ok {
		t.Error("其他 IP 不应受影响")
	}
}

// 窗口过期后计数清零。
func TestLimiterWindowExpires(t *testing.T) {
	l, now := newTestLimiter(LimiterConfig{IPLimit: 2, Window: time.Minute})

	l.Failure("1.1.1.1", "alice")
	l.Failure("1.1.1.1", "alice")
	if ok, _ := l.Allow("1.1.1.1"); ok {
		t.Fatal("超限 IP 应被拒绝")
	}

	*now = now.Add(2 * time.Minute)

	if ok, _ := l.Allow("1.1.1.1"); !ok {
		t.Error("窗口过期后应恢复放行")
	}
}

// 登录成功清除账号维度计数，但**不清除 IP 维度**——
// 否则攻击者用一个已知正确的账号登录一次即可重置爆破计数。
func TestLimiterSuccessClearsAccountButNotIP(t *testing.T) {
	l, _ := newTestLimiter(LimiterConfig{IPLimit: 5, Window: time.Minute})

	l.Failure("1.1.1.1", "alice")
	l.Failure("1.1.1.1", "alice")
	l.Success("alice")

	if d := l.Delay("alice"); d != 0 {
		t.Errorf("成功后账号延迟应清零, 实际 %v", d)
	}

	// IP 计数仍保留：再失败 3 次应触及阈值 5。
	l.Failure("1.1.1.1", "bob")
	l.Failure("1.1.1.1", "bob")
	l.Failure("1.1.1.1", "bob")
	if ok, _ := l.Allow("1.1.1.1"); ok {
		t.Error("IP 计数被登录成功重置了")
	}
}

// 账号维度采用递增延迟而非硬锁：达到阈值后仍放行，只是变慢。
func TestLimiterAccountDelayIncreases(t *testing.T) {
	l, _ := newTestLimiter(LimiterConfig{
		AccountLimit: 2,
		MaxDelay:     time.Second,
		Window:       time.Minute,
	})

	if d := l.Delay("alice"); d != 0 {
		t.Errorf("未失败时应无延迟, 实际 %v", d)
	}

	l.Failure("1.1.1.1", "alice")
	if d := l.Delay("alice"); d != 0 {
		t.Errorf("未达阈值时应无延迟, 实际 %v", d)
	}

	l.Failure("1.1.1.1", "alice")
	d1 := l.Delay("alice")
	if d1 <= 0 {
		t.Fatal("达到阈值后应有延迟")
	}

	l.Failure("1.1.1.1", "alice")
	if d2 := l.Delay("alice"); d2 <= d1 {
		t.Errorf("延迟应递增: %v -> %v", d1, d2)
	}

	// 逼近上限后不再增长。
	for i := 0; i < 20; i++ {
		l.Failure("1.1.1.1", "alice")
	}
	if d := l.Delay("alice"); d > time.Second {
		t.Errorf("延迟超过上限: %v", d)
	}

	// 关键：延迟不阻止登录成功——账号没有被锁死。
	if ok, _ := l.Allow("9.9.9.9"); !ok {
		t.Error("账号延迟不应影响其他来源的放行")
	}
}

// 记录数超过上限时应清理过期项，避免被大量伪造 IP 撑爆内存。
func TestLimiterEvictsExpiredEntries(t *testing.T) {
	l, now := newTestLimiter(LimiterConfig{Window: time.Minute, MaxEntries: 10})

	for i := 0; i < 20; i++ {
		l.Failure(string(rune('a'+i))+"-ip", "u")
	}

	*now = now.Add(2 * time.Minute)
	l.Failure("trigger", "u") // 触发清理

	l.mu.Lock()
	size := len(l.records)
	l.mu.Unlock()

	if size > 10 {
		t.Errorf("过期记录未被清理, 当前 %d 条", size)
	}
}
