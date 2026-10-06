// Package auth 实现认证：密码哈希、令牌签发与会话管理。
//
// 规则以 docs/07-specs/f-1-01-auth-session.md 为准。两条最容易写错、
// 也最关键的约束：
//
//   - 令牌载荷**不含角色与权限**（R-004），授权每次查库（f-1-06 R-011）；
//   - 令牌校验**必须同时校验签名与会话状态**（R-005），签名有效但会话
//     已撤销的请求必须拒绝。
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidToken 表示令牌无效：签名不符、已过期或格式错误。
//
// **刻意不区分具体原因**——对外一律按「未认证」处理，避免向攻击者
// 透露令牌是过期还是被伪造（f-1-01 §4「错误响应不泄漏判定细节」）。
var ErrInvalidToken = errors.New("令牌无效")

// Claims 是令牌载荷。
//
// 按 f-1-01 R-004，**只包含**会话标识、用户标识、令牌类型与过期时间。
// 角色不放进令牌：用户角色变更后应立即生效，而令牌在有效期内不会更新。
type Claims struct {
	SessionID string `json:"sid"`
	UserID    int64  `json:"uid"`
	TokenType string `json:"typ"`
	// KeyID 记录这枚令牌是用哪把密钥签的（轮换后可用于定位"它属于哪一代"）。
	//
	// 它进的是标准 kid 头而不是自定义字段：JWT 对此有既定位置，放在载荷里
	// 会成为第二个事实来源。
	jwt.RegisteredClaims
}

// TokenIssuer 负责签发与校验令牌。
type TokenIssuer struct {
	secret []byte
	// keyProvider 返回当前在用的密钥。装配后**每次签发/校验都重新取一次**
	// ——轮换是"改一个地方就立刻全员失效"的操作，缓存一把旧密钥会让轮换
	// 看起来没生效，而那是最危险的一种错觉。
	keyProvider func() (keyID string, secret []byte, err error)
}

// NewTokenIssuer 构造签发器。
//
// 密钥长度不足时直接失败，避免以弱密钥启动——签名密钥是会话安全的根基，
// 且支持轮换（f-1-01 R-012：更换密钥即让全部旧令牌失效）。
func NewTokenIssuer(secret string) (*TokenIssuer, error) {
	if len(secret) < minSecretLen {
		return nil, errors.New("签名密钥长度不得少于 32 字节")
	}
	return &TokenIssuer{secret: []byte(secret)}, nil
}

// SetKeyProvider 装配可轮换的密钥来源。
//
// 装配后 secret 字段不再使用；未装配时沿用构造时的静态密钥（单测与不启用
// 轮换的部署走这条路）。
func (t *TokenIssuer) SetKeyProvider(fn func() (string, []byte, error)) { t.keyProvider = fn }

// currentKey 取当前密钥。
func (t *TokenIssuer) currentKey() (string, []byte, error) {
	if t.keyProvider != nil {
		return t.keyProvider()
	}
	if len(t.secret) == 0 {
		return "", nil, errors.New("未配置签名密钥")
	}
	return "", t.secret, nil
}

// minSecretLen 是签名密钥的最小长度（字节）。
const minSecretLen = 32

// Issue 签发令牌，有效期由 expiresAt 决定（与会话的过期时间保持一致）。
func (t *TokenIssuer) Issue(sessionID string, userID int64, tokenType string, expiresAt time.Time) (string, error) {
	claims := Claims{
		SessionID: sessionID,
		UserID:    userID,
		TokenType: tokenType,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	keyID, secret, err := t.currentKey()
	if err != nil {
		return "", err
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	if keyID != "" {
		token.Header["kid"] = keyID
	}
	return token.SignedString(secret)
}

// Parse 校验签名与有效期并解析令牌。
//
// 显式限定签名算法，防止 alg 混淆攻击（如伪造 alg=none 或改用非对称算法）。
// 任何失败都返回 ErrInvalidToken，不暴露具体原因。
//
// 注意：本方法**只做令牌自身校验**，不检查会话状态——调用方必须再用
// SessionID 查一次会话（f-1-01 R-005）。
func (t *TokenIssuer) Parse(token string) (*Claims, error) {
	_, secret, err := t.currentKey()
	if err != nil {
		return nil, ErrInvalidToken
	}
	var claims Claims
	_, err = jwt.ParseWithClaims(token, &claims,
		func(*jwt.Token) (any, error) { return secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &claims, nil
}
