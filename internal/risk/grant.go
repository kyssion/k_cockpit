package risk

import (
	"sync"
	"time"
)

// Grant 是一次性许可，证明「某次高风险操作已通过验证」。
type Grant struct {
	Token     string
	UserID    int64
	SessionID int64
	Method    Method
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// GrantTTL 是许可的有效期。
//
// 2 分钟足够「验证完 → 重放原请求」，又不至于长到被截获后仍有价值。
//
// 刻意**不做「一段时间内免验证」**：那等于用户验证一次后就能连续执行任意
// 多次删除，二次确认的意义随之消失。而批量操作本就是一次请求，一次性许可
// 不会带来重复验证的困扰。
const GrantTTL = 2 * time.Minute

// Store 保存未消费的许可。
//
// **内存存储**（Q-009）：有效期仅 2 分钟且消费即失效，落库会带来无谓的
// 写入与清理负担。
//
// 代价需要明确：多实例部署时许可不共享，请求必须粘性路由到同一实例，
// 否则会出现「验证通过了却仍返回 428」。本期为单实例部署，此约束成立；
// 一旦改为多实例，这里需要换成共享存储（Redis 或进程外缓存）。
type Store struct {
	mu     sync.Mutex
	grants map[string]*Grant
}

// NewStore 构造许可存储。
func NewStore() *Store {
	return &Store{grants: make(map[string]*Grant)}
}

// Issue 签发一个一次性许可。
func (s *Store) Issue(userID, sessionID int64, method Method, now time.Time) (*Grant, error) {
	token, err := randomToken()
	if err != nil {
		return nil, err
	}

	g := &Grant{
		Token:     token,
		UserID:    userID,
		SessionID: sessionID,
		Method:    method,
		IssuedAt:  now,
		ExpiresAt: now.Add(GrantTTL),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(now)
	s.grants[token] = g
	return g, nil
}

// Consume 校验并**消费**许可，通过时返回该许可，否则返回 nil。
//
// 三种失败原因（不存在、已过期、归属不符）一律返回 nil：调用方只会据此
// 返回 428，不需要（也不应该）区分——区分会让调用方有机会把细节写进响应，
// 从而泄漏「这个令牌曾经存在过」这类信息。
//
// 返回许可本身而非 bool，是为了让调用方拿到**验证方式**用于审计（R-011）：
// 事后追溯需要知道「靠什么通过的验证」，而不只是「通过了」。
//
// **消费即失效**：许可在校验通过的同时被删除，因此同一令牌的第二次使用
// 必然失败。这是把滥用窗口压到最小的关键（Q-004）。
func (s *Store) Consume(token string, userID, sessionID int64, now time.Time) *Grant {
	if token == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.grants[token]
	if !ok {
		return nil
	}
	// 无论后续判断结果如何都先移除：一个已过期的许可留在表里，只会让
	// 「下次是否命中」变得不可预期。
	delete(s.grants, token)

	if now.After(g.ExpiresAt) {
		return nil
	}
	// 绑定会话与发起者（R-005）：许可不可转让、不可跨会话使用。
	// 仅比对用户不够——同一用户的另一个会话拿到令牌同样能用，那等于把
	// 「当前会话已通过验证」偷换成「这个人曾通过验证」。
	if g.UserID != userID || g.SessionID != sessionID {
		return nil
	}
	return g
}

// RevokeSession 丢弃某会话下的全部未消费许可，返回丢弃数量。
//
// 会话失效（登出、被撤销、安全信息变更）时调用（R-013）：否则一个已经
// 登出的会话仍可能在许可有效期内消费掉它——用户会看到「已登出却操作成功」。
func (s *Store) RevokeSession(sessionID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for token, g := range s.grants {
		if g.SessionID == sessionID {
			delete(s.grants, token)
			removed++
		}
	}
	return removed
}

// cleanupLocked 清理已过期的许可。调用方须持有锁。
//
// 惰性清理而非后台定时任务：许可数量天然很少（每人每次操作至多一个），
// 在签发时顺手扫一遍足够，不必为此引入一个常驻 goroutine。
func (s *Store) cleanupLocked(now time.Time) {
	for token, g := range s.grants {
		if now.After(g.ExpiresAt) {
			delete(s.grants, token)
		}
	}
}
