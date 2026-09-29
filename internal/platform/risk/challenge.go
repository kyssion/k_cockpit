package risk

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Challenge 标识「针对某次高风险操作的验证流程」。
//
// 它由服务端用 HMAC **自签名**而非落库：内容（用户、会话、动作、签发时间）
// 全在令牌里，服务端无需存储即可验证其真实性与有效期。为 2 分钟生命周期的
// 数据引入表结构与清理任务属于过度设计（与 Q-009 同一取舍）。
//
// 注意 Challenge **不是许可**：它只表示「有人发起了针对某操作的验证」，
// 拿到它并不能执行操作；能执行的是一次性的 Grant。
type Challenge struct {
	UserID    int64  `json:"u"`
	SessionID int64  `json:"s"`
	Action    Action `json:"a"`
	IssuedAt  int64  `json:"iat"`
}

// challengeTTL 是挑战的有效期。
//
// 它比许可有效期长：用户需要时间打开验证器、读取动态码并输入。太短会让
// 慢一步的用户收到「验证状态已失效」，太长则失去意义。
const challengeTTL = 5 * time.Minute

// ErrChallengeInvalid 表示挑战不可用（签名不符、格式错、已过期）。
//
// 对外一律映射为同一段文案（R-009 的同源考虑）：区分「过期」与「伪造」
// 会给探测者提供判断依据。
var ErrChallengeInvalid = errors.New("challenge invalid")

type signer struct {
	key []byte
}

func newSigner(key []byte) *signer {
	return &signer{key: key}
}

// Issue 签发一个挑战。
func (s *signer) Issue(userID, sessionID int64, action Action, now time.Time) (string, error) {
	payload, err := json.Marshal(Challenge{
		UserID:    userID,
		SessionID: sessionID,
		Action:    action,
		IssuedAt:  now.Unix(),
	})
	if err != nil {
		return "", err
	}

	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + s.sign(body), nil
}

// Parse 校验并解析挑战。
func (s *signer) Parse(token string, now time.Time) (*Challenge, error) {
	body, sig, found := cutLast(token, '.')
	if !found {
		return nil, ErrChallengeInvalid
	}

	// 先验签再解析内容：让攻击者构造的畸形载荷在进入 JSON 解析前就被拒。
	// 用常量时间比较，避免通过响应耗时逐字节猜测签名。
	if !hmac.Equal([]byte(sig), []byte(s.sign(body))) {
		return nil, ErrChallengeInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrChallengeInvalid
	}

	var c Challenge
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrChallengeInvalid
	}
	if now.Sub(time.Unix(c.IssuedAt, 0)) > challengeTTL {
		return nil, ErrChallengeInvalid
	}
	// 拒绝「未来签发」的挑战：签发时间异常说明签名密钥可能已泄漏，
	// 此时放行等于承认一个来源不明的挑战。
	if time.Unix(c.IssuedAt, 0).After(now.Add(time.Minute)) {
		return nil, ErrChallengeInvalid
	}
	return &c, nil
}

func (s *signer) sign(body string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// cutLast 按最后一个分隔符切分。
func cutLast(s string, sep byte) (before, after string, found bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// randomToken 生成 32 字节的随机令牌（许可与恢复码共用）。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
