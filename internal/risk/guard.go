package risk

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// GrantHeader 是重放原请求时携带许可的请求头。
//
// 用自定义头而非 Cookie：许可只对**紧接着的那一次重放**有意义，不应被
// 浏览器自动附加到后续所有请求上（那会让「一次性」在实际使用中变成
// 「一段时间内全程有效」）。
const GrantHeader = "X-Risk-Grant"

// ctxKeyVerifiedMethod 存本次请求已通过的验证方式，供 handler 写审计（R-011）。
const ctxKeyVerifiedMethod = "risk_verified_method"

// Guard 强制高风险操作的二次验证。
type Guard struct {
	signer  *signer
	grants  *Store
	limiter *attemptLimiter
	db      *gorm.DB
	audit   *audit.Recorder
	// encKey 用于加解密 TOTP 密钥（列名 totp_secret_enc 即约定加密存储）。
	encKey []byte
}

// NewGuard 构造验证守卫。
//
// secret 是根密钥，内部按用途派生出签名密钥与加密密钥：复用同一把密钥做
// 签名与加密，会让两者中任一处的弱点波及另一处。
func NewGuard(db *gorm.DB, secret []byte, recorder *audit.Recorder) *Guard {
	return &Guard{
		signer:  newSigner(deriveKey(secret, labelSign)),
		encKey:  deriveKey(secret, labelEnc),
		grants:  NewStore(),
		limiter: newAttemptLimiter(),
		db:      db,
		audit:   recorder,
	}
}

// RequiredData 是 428 响应中 data 字段的内容。
type RequiredData struct {
	Action      Action       `json:"action"`
	ChallengeID string       `json:"challenge_id"`
	Methods     []MethodInfo `json:"methods"`
	ExpiresAt   time.Time    `json:"expires_at"`
}

// MethodInfo 描述一种可用的验证方式。
type MethodInfo struct {
	Method string `json:"method"`
	Label  string `json:"label"`
}

// Require 检查请求是否携带有效许可。
//
// 通过返回 true；否则写入 428 响应并返回 false，调用方直接返回即可。
//
// 调用方**不需要**先判断 Protected：不在清单内的操作会直接放行，这样
// 接入点可以统一写成一行，不会因为漏判而把普通操作也拦下来。
func (g *Guard) Require(c *app.RequestContext, action Action) bool {
	if !Protected(action) {
		return true
	}

	user := auth.CurrentUser(c)
	session := auth.CurrentSession(c)
	if user == nil || session == nil {
		api.Fail(c, api.Unauthenticated("请先登录"))
		return false
	}

	token := string(c.GetHeader(GrantHeader))
	if grant := g.grants.Consume(token, user.ID, session.ID, time.Now()); grant != nil {
		// 记下验证方式，供 handler 写审计（R-011）：事后追溯需要知道
		// 「这次操作是靠什么通过验证的」，而不只是「通过了」。
		c.Set(ctxKeyVerifiedMethod, string(grant.Method))
		return true
	}

	g.writeRequired(c, user, session, action)
	return false
}

// VerifiedMethodOf 返回本次请求已通过的验证方式；未通过时为空串。
func VerifiedMethodOf(c *app.RequestContext) string {
	if v, ok := c.Get(ctxKeyVerifiedMethod); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// writeRequired 写出 428 响应。
func (g *Guard) writeRequired(
	c *app.RequestContext, user *model.User, session *model.Session, action Action,
) {
	now := time.Now()
	methods := AvailableMethods(user)

	infos := make([]MethodInfo, 0, len(methods))
	for _, m := range methods {
		infos = append(infos, MethodInfo{Method: string(m), Label: m.Label()})
	}

	// 无可用方式时仍然签发 challenge：前端据此提示「需先绑定」，而不是
	// 让用户面对一个没有出口的 428。
	challengeID, err := g.signer.Issue(user.ID, session.ID, action, now)
	if err != nil {
		api.Fail(c, api.Internal())
		return
	}

	message := "该操作需要完成二次验证"
	if len(methods) == 0 {
		message = "该操作需要二次验证，但当前账号尚未绑定验证方式，请先绑定"
	}

	api.FailWithData(c, api.RiskVerificationRequired(message), RequiredData{
		Action:      action,
		ChallengeID: challengeID,
		Methods:     infos,
		ExpiresAt:   now.Add(challengeTTL),
	})
}

// VerifyRequest 是验证请求。
type VerifyRequest struct {
	ChallengeID string
	Method      string
	Code        string
}

// Verify 处理验证请求并签发一次性许可。
func (g *Guard) Verify(
	ctx context.Context, c *app.RequestContext,
	user *model.User, session *model.Session, req VerifyRequest,
) (*Grant, error) {
	// 先看限流再看验证码：次数用尽后连校验都不做，避免通过响应耗时或
	// 错误文案的细微差异继续试探。
	if allowed, retryAfter := g.limiter.allow(user.ID); !allowed {
		return nil, api.RateLimited(
			fmt.Sprintf("验证尝试过于频繁，请在 %d 秒后重试", int(retryAfter.Seconds())),
		)
	}

	now := time.Now()
	challenge, err := g.signer.Parse(req.ChallengeID, now)
	if err != nil {
		// 挑战失效与「验证码错误」用不同文案：前者是流程问题（重新发起操作
		// 即可），后者需要用户重输。都笼统说「验证失败」会让用户反复重输
		// 一个永远不可能通过的验证码。
		return nil, api.ValidationFailed("验证状态已失效，请重新发起操作")
	}
	// 许可与挑战都绑定会话与发起者（R-005）：否则同一用户的另一个会话
	// 拿到 challenge 也能完成验证，等于把「这个会话验证过」偷换成
	// 「这个人验证过」。
	if challenge.UserID != user.ID || challenge.SessionID != session.ID {
		return nil, api.ValidationFailed("验证状态已失效，请重新发起操作")
	}

	method := Method(req.Method)
	if !method.valid() || !slices.Contains(AvailableMethods(user), method) {
		return nil, api.InvalidParameter("不支持的验证方式")
	}

	// TOTP 密钥在库中是加密存储的（列名 totp_secret_enc），校验前必须解密。
	// 恢复码不需要这一步。
	secret := ""
	if method == MethodTOTP && user.TotpSecretEnc != nil && *user.TotpSecretEnc != "" {
		plain, err := openSecret(g.encKey, *user.TotpSecretEnc)
		if err != nil {
			// 解密失败通常意味着根密钥变了（重启时未持久化配置）。
			// 这会让所有用户无法通过验证——必须留下日志，否则只会表现为
			// 一堆「验证码不正确」的投诉。
			log.Printf("[risk] 解密 TOTP 密钥失败 user=%d: %v", user.ID, err)
		} else {
			secret = plain
		}
	}

	result := Verify(user, secret, method, req.Code, now)
	if !result.OK {
		g.limiter.failure(user.ID)
		remaining := g.limiter.remaining(user.ID)

		// 统一文案（R-009）：不区分「码错误」与「码过期」——区分会帮攻击者
		// 判断自己的猜测是否接近。附带剩余次数（R-006）是必要的体验，
		// 它只暴露「还剩几次」，不暴露「错在哪」。
		g.record(ctx, user, session, challenge.Action, string(method), "risk.verify.failed", false, remaining)
		return nil, api.ValidationFailed(
			fmt.Sprintf("验证码不正确，还可尝试 %d 次", remaining),
		)
	}

	// 恢复码消费后必须落库（R-008）：不写回等于这个码还能再用一次。
	if method == MethodRecoveryCode {
		if err := g.saveRecoveryCodes(ctx, user.ID, result.UpdatedRecoveryHash); err != nil {
			return nil, err
		}
	}

	// 成功即清零失败计数：否则长期使用中累积的偶发手误会在某天突然把
	// 用户锁在门外，而他完全不知道自己「什么时候犯过错」。
	g.limiter.success(user.ID)

	grant, err := g.grants.Issue(user.ID, session.ID, method, now)
	if err != nil {
		log.Printf("[risk] 签发许可失败: %v", err)
		return nil, api.Internal()
	}

	g.record(ctx, user, session, challenge.Action, string(method), "risk.verify.success", true, result.RemainingRecoveryCodes)
	return grant, nil
}

// RevokeSession 丢弃某会话下的未消费许可（R-013）。
//
// 在登出与会话撤销时调用：否则一个已经登出的会话仍可能在许可有效期内
// 消费掉它，用户会看到「已登出却操作成功」。
func (g *Guard) RevokeSession(sessionID int64) int {
	return g.grants.RevokeSession(sessionID)
}

// Expire 返回许可有效期，供接口返回给前端展示倒计时。
func (g *Guard) Expire() time.Duration { return GrantTTL }

// saveRecoveryCodes 写回消费后的恢复码哈希。
func (g *Guard) saveRecoveryCodes(ctx context.Context, userID int64, hashes string) error {
	err := g.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", userID).
		Update("recovery_codes_hash", hashes).Error
	if err != nil {
		log.Printf("[risk] 更新恢复码失败 user=%d: %v", userID, err)
		return api.Internal()
	}
	return nil
}

// record 写审计（R-011）。
func (g *Guard) record(
	ctx context.Context, user *model.User, session *model.Session,
	action Action, method, auditAction string, success bool, remaining int,
) {
	if g.audit == nil {
		return
	}

	after := map[string]any{
		"method":     method,
		"session_id": session.ID,
	}
	if remaining > 0 {
		after["remaining_recovery_codes"] = remaining
	}

	g.audit.Record(ctx, audit.Entry{
		OperatorID:   user.ID,
		OperatorName: user.Username,
		ResourceType: "security",
		ResourceName: string(action),
		Action:       auditAction,
		Success:      success,
		AfterState:   after,
	})
}
