package auth_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func newIssuer(t *testing.T) *auth.TokenIssuer {
	t.Helper()
	issuer, err := auth.NewTokenIssuer(testSecret)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	return issuer
}

func TestIssueAndParse(t *testing.T) {
	issuer := newIssuer(t)
	expires := time.Now().Add(time.Hour).Truncate(time.Second)

	token, err := issuer.Issue("sess-1", 42, model.TokenTypeAccess, expires)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	claims, err := issuer.Parse(token)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if claims.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, 期望 sess-1", claims.SessionID)
	}
	if claims.UserID != 42 {
		t.Errorf("UserID = %d, 期望 42", claims.UserID)
	}
	if claims.TokenType != model.TokenTypeAccess {
		t.Errorf("TokenType = %q, 期望 %q", claims.TokenType, model.TokenTypeAccess)
	}
	if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.Equal(expires) {
		t.Errorf("ExpiresAt = %v, 期望 %v", claims.ExpiresAt, expires)
	}
}

// R-004：载荷不得包含角色——角色变更后应立即生效，不能等令牌过期。
func TestClaimsCarryNoRole(t *testing.T) {
	issuer := newIssuer(t)
	token, err := issuer.Issue("s", 1, model.TokenTypeAccess, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatalf("解析载荷失败: %v", err)
	}
	for _, forbidden := range []string{"admin", "tenant", "role", "perm", "scope"} {
		if strings.Contains(string(payload), forbidden) {
			t.Errorf("令牌载荷包含不应出现的字段 %q: %s", forbidden, payload)
		}
	}
}

func TestParseRejectsExpired(t *testing.T) {
	issuer := newIssuer(t)

	token, err := issuer.Issue("s", 1, model.TokenTypeAccess, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	if _, err := issuer.Parse(token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("过期令牌应返回 ErrInvalidToken, 实际 %v", err)
	}
}

func TestParseRejectsTampered(t *testing.T) {
	issuer := newIssuer(t)
	token, err := issuer.Issue("s", 1, model.TokenTypeAccess, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("令牌格式异常: %s", token)
	}

	// 篡改载荷（把 uid 换掉），签名不变——必须被拒绝。
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"s","uid":999,"typ":"access"}`))
	tampered := parts[0] + "." + tamperedPayload + "." + parts[2]

	if _, err := issuer.Parse(tampered); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("篡改载荷的令牌应被拒绝, 实际 %v", err)
	}

	// 篡改签名
	if _, err := issuer.Parse(parts[0] + "." + parts[1] + ".AAAA"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("篡改签名的令牌应被拒绝, 实际 %v", err)
	}
}

// 换一个密钥后旧令牌必须失效（R-012：密钥轮换即让全部旧令牌失效）。
func TestParseRejectsForeignSecret(t *testing.T) {
	issuer := newIssuer(t)
	token, err := issuer.Issue("s", 1, model.TokenTypeAccess, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	other, err := auth.NewTokenIssuer("ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	if _, err := other.Parse(token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("异密钥签发的令牌应被拒绝, 实际 %v", err)
	}
}

// 防 alg 混淆攻击：拒绝声明 alg=none 的令牌。
func TestParseRejectsNoneAlgorithm(t *testing.T) {
	issuer := newIssuer(t)

	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sid":"s","uid":1,"typ":"access","exp":99999999999}`))
	noneToken := header + "." + payload + "."

	if _, err := issuer.Parse(noneToken); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("alg=none 的令牌应被拒绝, 实际 %v", err)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	issuer := newIssuer(t)

	for _, bad := range []string{"", "not-a-token", "a.b", "a.b.c.d"} {
		if _, err := issuer.Parse(bad); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("非法令牌 %q 应返回 ErrInvalidToken, 实际 %v", bad, err)
		}
	}
}

// 弱密钥必须直接拒绝启动，避免以可暴力破解的密钥签发令牌。
func TestNewTokenIssuerRejectsWeakSecret(t *testing.T) {
	for _, weak := range []string{"", "short", strings.Repeat("a", 31)} {
		if _, err := auth.NewTokenIssuer(weak); err == nil {
			t.Errorf("长度 %d 的密钥应被拒绝", len(weak))
		}
	}
}

// 无效令牌的错误文案不得泄漏内部判定细节。
func TestInvalidTokenErrorIsOpaque(t *testing.T) {
	msg := auth.ErrInvalidToken.Error()
	for _, leak := range []string{"expired", "signature", "alg", "decode"} {
		if strings.Contains(strings.ToLower(msg), leak) {
			t.Errorf("错误文案泄漏了判定细节 %q: %s", leak, msg)
		}
	}
}
