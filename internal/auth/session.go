package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// errInvalidCredentials 是登录失败的统一对外错误。
//
// 按 f-1-01 R-002，用户不存在、密码错误、账号封禁、账号未激活**共用同一
// 文案与状态码**：任何差异都可被用来枚举用户名。
var errInvalidCredentials = api.Unauthenticated("用户名或密码错误")

// errUnauthenticated 是会话校验失败的统一对外错误。
//
// 同样不区分「令牌过期」「签名无效」「会话已撤销」「指纹不匹配」——
// 判定细节只进审计，不给请求方（f-1-01 §4）。
var errUnauthenticated = api.Unauthenticated("登录状态已失效，请重新登录")

// Config 是认证服务的可调参数。
type Config struct {
	// IdleTimeout 是空闲超时，从最后一次**真实用户活动**起算（R-009）。
	IdleTimeout time.Duration
	// AbsoluteTimeout 是绝对上限，从会话签发起算：无论多活跃都会到期，
	// 用于限制令牌被长期窃用的窗口。
	AbsoluteTimeout time.Duration
	// ActivityThrottle 是活动时间的最小写入间隔。
	// 每个请求都写库会造成写放大，因此仅在距上次更新超过该间隔时才落库。
	ActivityThrottle time.Duration
}

// DefaultConfig 返回默认参数。
func DefaultConfig() Config {
	return Config{
		IdleTimeout:      2 * time.Hour,
		AbsoluteTimeout:  7 * 24 * time.Hour,
		ActivityThrottle: time.Minute,
	}
}

// ClientInfo 是发起请求的客户端信息，参与会话指纹计算（R-007）并记入审计。
type ClientInfo struct {
	IP        string
	UserAgent string
}

// Activity 表示本次请求是否计为「真实用户活动」（R-008）。
//
// 只有导航、显式交互与写操作算真实活动；**轮询与实时通道心跳不算**——
// 否则用户开着页面不动也会让会话无限续期，空闲超时形同虚设。
type Activity bool

const (
	// Passive 是轮询、SSE 心跳等非用户触发的请求。
	Passive Activity = false
	// Real 是用户真实操作。
	Real Activity = true
)

// LoginResult 是登录成功的结果。
//
// Stage 告诉前端"下一步做什么"。把它放在结果里而不是让前端自己判断：
// 前端要同时拿到 totp_enabled、force_password_change、角色等信息才能推出
// 同一个结论，而任何一个字段的口径变了（例如将来要求普通用户也绑邮箱），
// 前端就会停在错误的那一步。
type LoginResult struct {
	Token     string
	ExpiresAt time.Time
	User      *model.User
	Stage     Stage
}

// Service 提供认证领域操作。
type Service struct {
	db     *gorm.DB
	tokens *TokenIssuer
	audit  *audit.Recorder
	// verifier 校验登录阶段的二次验证码，见 LoginFactorVerifier。
	verifier LoginFactorVerifier
	// mailer 提供发信能力（邮箱绑定、找回密码）。未注入时相关接口降级为
	// "尚未配置邮件服务"，而不是 500。
	mailer  Mailer
	limiter *Limiter
	cfg     Config
}

// NewService 构造认证服务。cfg 为零值时使用默认参数。
func NewService(db *gorm.DB, tokens *TokenIssuer, recorder *audit.Recorder, cfg Config) *Service {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultConfig().IdleTimeout
	}
	if cfg.AbsoluteTimeout <= 0 {
		cfg.AbsoluteTimeout = DefaultConfig().AbsoluteTimeout
	}
	if cfg.ActivityThrottle <= 0 {
		cfg.ActivityThrottle = DefaultConfig().ActivityThrottle
	}
	return &Service{
		db:      db,
		tokens:  tokens,
		audit:   recorder,
		limiter: NewLimiter(DefaultLimiterConfig()),
		cfg:     cfg,
	}
}

// Login 校验凭据并建立会话。
//
// 失败时一律返回 errInvalidCredentials，不区分原因（R-002）。
func (s *Service) Login(ctx context.Context, username, password string, ci ClientInfo) (*LoginResult, error) {
	// IP 维度限流：超限时连密码都不校验，避免被用于爆破（R-003）。
	if ok, retry := s.limiter.Allow(ci.IP); !ok {
		return nil, api.RateLimited(fmt.Sprintf("登录尝试过于频繁，请 %d 秒后重试", int(retry.Seconds())+1))
	}

	// 账号维度递增延迟（Q-005）：只拖慢尝试速度，不锁定账号——
	// 否则攻击者可用错误密码把任意用户锁在门外。
	if d := s.limiter.Delay(username); d > 0 {
		time.Sleep(d)
	}

	var user model.User
	err := s.db.WithContext(ctx).Where("username = ?", username).First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 用户不存在时也做一次等价校验：否则响应时间会明显短于
		// 「用户存在但密码错误」，用户名依然可被枚举。
		_, _ = VerifyPassword(dummyHash, password)
		s.limiter.Failure(ci.IP, username)
		s.recordLogin(ctx, nil, username, ci, false, "凭据无效")
		return nil, errInvalidCredentials
	case err != nil:
		// Q-007：认证是安全边界，依赖不可用时宁可 503 也不降级放行。
		log.Printf("[auth] 查询用户失败: %v", err)
		return nil, api.Unavailable("认证服务暂时不可用")
	}

	okPw, err := VerifyPassword(user.PasswordHash, password)
	if err != nil {
		// 哈希串损坏属于数据问题，不是「密码错误」，需与凭据失败区分开。
		log.Printf("[auth] 用户 %d 的密码哈希无效: %v", user.ID, err)
		return nil, api.Internal()
	}
	if !okPw {
		s.limiter.Failure(ci.IP, username)
		s.recordLogin(ctx, &user, username, ci, false, "凭据无效")
		return nil, errInvalidCredentials
	}

	// 账号状态不满足时仍返回统一文案，不暴露「已被封禁 / 未激活」。
	if !user.IsActive() {
		s.limiter.Failure(ci.IP, username)
		s.recordLogin(ctx, &user, username, ci, false, "账号状态不允许登录")
		return nil, errInvalidCredentials
	}

	s.limiter.Success(username)
	s.recordLogin(ctx, &user, username, ci, true, "")

	// 凭据通过不等于登录完成：还有强制改密、二次验证与安全初始化三个阶段
	// 要走。见 startFlow。
	return s.startFlow(ctx, &user, ci)
}

// issueSession 建立一条会话并签发对应级别的令牌。
//
// ttl 为正时用它作为过期时间（登录阶段用），否则沿用空闲/绝对超时规则。
// 两个阶段共用这一段：它们的差别只在令牌级别与寿命，分开写两份迟早会
// 在其中一份漏掉指纹或审计。
func (s *Service) issueSession(
	ctx context.Context, user *model.User, tokenType string, ttl time.Duration, ci ClientInfo, stage Stage,
) (*LoginResult, error) {
	now := time.Now()
	sessionID, err := newSessionID()
	if err != nil {
		log.Printf("[auth] 生成会话标识失败: %v", err)
		return nil, api.Internal()
	}
	expiresAt := s.expiry(now, now)
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}

	session := model.Session{
		SessionID:    sessionID,
		UserID:       user.ID,
		TokenType:    tokenType,
		Fingerprint:  strPtr(Fingerprint(ci.IP, ci.UserAgent)),
		ClientIP:     strPtr(ci.IP),
		UserAgent:    strPtr(truncate(ci.UserAgent, 255)),
		IssuedAt:     now,
		ExpiresAt:    expiresAt,
		LastActiveAt: &now,
	}
	if err := s.db.WithContext(ctx).Create(&session).Error; err != nil {
		log.Printf("[auth] 创建会话失败: %v", err)
		return nil, api.Internal()
	}

	token, err := s.tokens.Issue(sessionID, user.ID, tokenType, expiresAt)
	if err != nil {
		log.Printf("[auth] 签发令牌失败: %v", err)
		return nil, api.Internal()
	}
	return &LoginResult{Token: token, ExpiresAt: expiresAt, User: user, Stage: stage}, nil
}

// Authenticate 校验令牌与会话状态，返回当前用户与会话。
//
// 令牌校验分两层，缺一不可（R-005）：签名有效但会话已撤销的请求必须拒绝。
// 用户与会话状态**每次查库**：角色变更、封禁、安全信息变更都要立即生效，
// 不能等令牌过期。
func (s *Service) Authenticate(ctx context.Context, token string, ci ClientInfo, activity Activity) (*model.User, *model.Session, error) {
	claims, err := s.tokens.Parse(token)
	if err != nil {
		return nil, nil, errUnauthenticated
	}

	var session model.Session
	err = s.db.WithContext(ctx).Where("session_id = ?", claims.SessionID).First(&session).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil, errUnauthenticated
	case err != nil:
		// Q-007：数据库不可用时返回 503，绝不「校验失败即放行」。
		log.Printf("[auth] 查询会话失败: %v", err)
		return nil, nil, api.Unavailable("认证服务暂时不可用")
	}

	now := time.Now()
	if session.IsRevoked() || session.IsExpired(now) {
		return nil, nil, errUnauthenticated
	}
	// 令牌与会话必须指向同一主体与同一级别：防止用 login 级令牌
	// 冒充 access 级会话（R-006）。
	if session.UserID != claims.UserID || session.TokenType != claims.TokenType {
		return nil, nil, errUnauthenticated
	}

	// 指纹比对（R-007）：不匹配即拒绝，宁可让 IP 变化的用户重新登录。
	if session.Fingerprint != nil && *session.Fingerprint != Fingerprint(ci.IP, ci.UserAgent) {
		s.recordSecurityEvent(ctx, &session, ci, "session.fingerprint_mismatch")
		return nil, nil, errUnauthenticated
	}

	var user model.User
	err = s.db.WithContext(ctx).Where("id = ?", session.UserID).First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil, errUnauthenticated
	case err != nil:
		log.Printf("[auth] 查询用户失败: %v", err)
		return nil, nil, api.Unavailable("认证服务暂时不可用")
	}

	// 封禁 / 软删除立即生效（R-011）。
	if !user.IsActive() {
		return nil, nil, errUnauthenticated
	}

	// 安全信息变更（改密 / 改用户名 / 2FA 变更）让全部既有会话失效（R-010）。
	if user.SecurityUpdatedAt != nil && user.SecurityUpdatedAt.After(session.IssuedAt) {
		return nil, nil, errUnauthenticated
	}

	if activity == Real {
		s.touch(ctx, &session, now)
	}

	return &user, &session, nil
}

// Logout 撤销指定会话（R-013：登出只影响当前会话）。
func (s *Service) Logout(ctx context.Context, sessionID string, ci ClientInfo) error {
	res := s.db.WithContext(ctx).Model(&model.Session{}).
		Where("session_id = ? AND revoked_at IS NULL", sessionID).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		log.Printf("[auth] 撤销会话失败: %v", res.Error)
		return api.Internal()
	}
	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			ResourceType: "session",
			Action:       "session.logout",
			Success:      true,
			ClientIP:     ci.IP,
		})
	}
	return nil
}

// Sessions 返回某用户的全部有效会话，最近签发的排在前面。
func (s *Service) Sessions(ctx context.Context, userID int64) ([]model.Session, error) {
	var sessions []model.Session
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", userID, time.Now()).
		Order("issued_at DESC").
		Find(&sessions).Error
	if err != nil {
		log.Printf("[auth] 查询会话列表失败: %v", err)
		return nil, api.Internal()
	}
	return sessions, nil
}

// RevokeOtherSessions 撤销该用户除指定会话之外的全部会话，返回撤销条数。
//
// 它服务于「改密码」：改密码最常见的动机就是"我怀疑账号被盗"，而如果旧会话
// 还活着，这个动作就失去了全部意义——攻击者的会话继续有效，用户却以为已经
// 把对方踢出去了。
//
// 用一个 UPDATE 而不是先查再逐条撤销：逐条需要 N 次往返，而中途失败会留下
// 「一部分撤销了、一部分没有」的中间状态——那种状态下用户无法判断自己是否
// 安全，而重试的结果也不确定。
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keepRowID int64) (int64, error) {
	res := s.db.WithContext(ctx).Model(&model.Session{}).
		Where("user_id = ? AND id <> ? AND revoked_at IS NULL", userID, keepRowID).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		log.Printf("[auth] 撤销其它会话失败: %v", res.Error)
		return 0, api.Internal()
	}
	return res.RowsAffected, nil
}

// RevokeSession 撤销指定会话，只能撤销自己名下的（他人会话返回 404）。
func (s *Service) RevokeSession(ctx context.Context, userID, sessionRowID int64, ci ClientInfo) error {
	res := s.db.WithContext(ctx).Model(&model.Session{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", sessionRowID, userID).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		log.Printf("[auth] 撤销会话失败: %v", res.Error)
		return api.Internal()
	}
	if res.RowsAffected == 0 {
		// 不存在与不属于自己表现一致，避免探测他人会话是否存在（f-1-06 R-005）。
		return api.NotFound("会话不存在")
	}
	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID:   userID,
			ResourceType: "session",
			ResourceID:   sessionRowID,
			Action:       "session.revoke",
			Success:      true,
			ClientIP:     ci.IP,
		})
	}
	return nil
}

// expiry 计算会话的过期时间：空闲超时与绝对上限取较早者（R-009）。
func (s *Service) expiry(now, issuedAt time.Time) time.Time {
	idle := now.Add(s.cfg.IdleTimeout)
	absolute := issuedAt.Add(s.cfg.AbsoluteTimeout)
	if idle.Before(absolute) {
		return idle
	}
	return absolute
}

// touch 按需更新会话的活跃时间与过期时间。
//
// 只有当距上次更新超过 ActivityThrottle 时才写库：否则每个请求都会产生
// 一次 UPDATE，在高频操作下形成写放大。
func (s *Service) touch(ctx context.Context, session *model.Session, now time.Time) {
	if session.LastActiveAt != nil && now.Sub(*session.LastActiveAt) < s.cfg.ActivityThrottle {
		return
	}

	expiresAt := s.expiry(now, session.IssuedAt)
	err := s.db.WithContext(ctx).Model(&model.Session{}).
		Where("session_id = ?", session.SessionID).
		Updates(map[string]any{"last_active_at": now, "expires_at": expiresAt}).Error
	if err != nil {
		// 续期失败不影响本次请求：会话仍在有效期内，下次请求会再尝试。
		log.Printf("[auth] 更新会话活跃时间失败: %v", err)
		return
	}

	session.LastActiveAt = &now
	session.ExpiresAt = expiresAt
}

func (s *Service) recordLogin(ctx context.Context, user *model.User, username string, ci ClientInfo, success bool, reason string) {
	if s.audit == nil {
		return
	}
	e := audit.Entry{
		Source:       model.SourceWeb,
		ResourceType: "session",
		ResourceName: username,
		Action:       "user.login",
		Success:      success,
		Error:        reason,
		ClientIP:     ci.IP,
	}
	if user != nil {
		e.OperatorID = user.ID
		e.OperatorName = user.Username
		e.ResourceID = user.ID
	}
	s.audit.Record(ctx, e)
}

// recordSecurityEvent 记录安全类事件（如指纹不匹配）。
//
// 它只写审计，不中断请求——调用方随后仍会返回统一错误。
func (s *Service) recordSecurityEvent(ctx context.Context, session *model.Session, ci ClientInfo, action string) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, audit.Entry{
		OperatorID:   session.UserID,
		ResourceType: "session",
		ResourceID:   session.ID,
		Action:       action,
		Success:      false,
		Error:        "会话指纹不匹配",
		ClientIP:     ci.IP,
	})
}

// dummyHash 用于「用户不存在」时的等价校验消耗，抹平响应时间差异。
//
// 初始化失败直接 panic：它意味着随机源或内存异常，带着这种状态继续运行
// 会让登录接口的可信度无从保证。
var dummyHash = func() string {
	h, err := HashPassword("timing-equalizer")
	if err != nil {
		panic("初始化密码哈希失败: " + err.Error())
	}
	return h
}()

// Fingerprint 计算会话指纹（R-007）。
//
// 由客户端 IP 与 User-Agent 摘要而成，取摘要而非原文，避免 UA 原文
// 散落在多处；32 位十六进制适配 varchar(128)。
func Fingerprint(ip, userAgent string) string {
	sum := sha256.Sum256([]byte(ip + "|" + userAgent))
	return hex.EncodeToString(sum[:16])
}

// newSessionID 生成会话标识：32 字节随机，64 位十六进制。
func newSessionID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成会话标识失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func strPtr(v string) *string { return &v }

// truncate 按字节截断字符串，用于适配有长度上限的列。
//
// User-Agent 由客户端控制，超长时不能依赖入库失败来暴露问题。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
