package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// 验证码参数。
const (
	// codeLength 是验证码位数。6 位是"手抄一遍不出错"与"猜中概率 1/100 万"
	// 之间的常见平衡；再加一位带来的收益远小于输入负担。
	codeLength = 6
	// codeTTL 是绑定邮箱验证码的有效期。
	codeTTL = 15 * time.Minute
	// resetCodeTTL 是找回密码验证码的有效期，比绑定更短：它最终能改密码。
	resetCodeTTL = 10 * time.Minute
	// resetTicketTTL 是校验通过后换发的重置票据有效期。
	//
	// 分成"验证码 + 票据"两步而不是让验证码直接改密码：验证码走邮件，
	// 中途可能被看到；票据只存在于这一次请求的返回值里。
	resetTicketTTL = 10 * time.Minute
	// maxCodeAttempts 是单个验证码的尝试上限。
	//
	// 6 位数字的空间只有一百万，且邮件可能被转发或误投递；没有次数上限
	// 的话，"猜"就是一个可行的攻击。
	maxCodeAttempts = 5
	// resendInterval 是同一邮箱同一用途的发码最小间隔。
	resendInterval = 60 * time.Second
)

// 邮件模板的场景名。
const (
	sceneBind  = "绑定邮箱"
	sceneReset = "找回密码"
)

// Mailer 是发信能力的最小接口。
//
// 只声明本文件用到的两个方法：由 internal/mailer 实现。收窄接口的好处是
// 测试里塞一个假实现只需要记下"发给了谁、内容是什么"。
type Mailer interface {
	Configured(ctx context.Context) (bool, error)
	SendVerificationCode(ctx context.Context, to, scene, code string) error
}

// SetMailer 注入发信能力；未注入时邮箱相关接口返回"邮件服务未配置"。
func (s *Service) SetMailer(m Mailer) { s.mailer = m }

// ErrMailNotConfigured 用于对外说明"没配邮件"。
var ErrMailNotConfigured = api.Unavailable("尚未配置邮件服务，请联系管理员")

// --- 已登录用户：绑定 / 改绑邮箱 ---

// SendEmailCode 向指定邮箱发送绑定验证码。
//
// 不校验该邮箱是否已被他人使用：判断"这个邮箱属于谁"需要先发信确认，
// 而提前拒绝就等于给了"枚举哪些邮箱注册过本面板"的接口。
func (s *Service) SendEmailCode(ctx context.Context, user *model.User, email string, ci ClientInfo) error {
	if s.mailer == nil {
		return ErrMailNotConfigured
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	if ok, err := s.mailer.Configured(ctx); err != nil || !ok {
		return ErrMailNotConfigured
	}
	return s.issueCode(ctx, user, email, model.EmailCodeBind, codeTTL, ci)
}

// ConfirmEmail 校验验证码并绑定邮箱。
func (s *Service) ConfirmEmail(ctx context.Context, user *model.User, email, code string) error {
	email, err := normalizeEmail(email)
	if err != nil {
		return err
	}
	if _, err := s.consumeCode(ctx, user, email, model.EmailCodeBind, code); err != nil {
		return err
	}
	now := time.Now()
	return s.UpdateUser(ctx, user.ID, map[string]any{
		"email":             email,
		"email_verified_at": now,
	})
}

// --- 登录中间态：安全初始化引导里绑邮箱 ---

// SendStagedEmailCode 在登录引导阶段发送绑定验证码。
func (s *Service) SendStagedEmailCode(
	ctx context.Context, token, email string, ci ClientInfo,
) error {
	user, _, err := s.AuthenticateStage(ctx, token, model.TokenTypeLogin, ci)
	if err != nil {
		return err
	}
	return s.SendEmailCode(ctx, user, email, ci)
}

// ConfirmStagedEmail 在登录引导阶段确认绑定。
func (s *Service) ConfirmStagedEmail(
	ctx context.Context, token, email, code string, ci ClientInfo,
) error {
	user, _, err := s.AuthenticateStage(ctx, token, model.TokenTypeLogin, ci)
	if err != nil {
		return err
	}
	return s.ConfirmEmail(ctx, user, email, code)
}

// --- 找回密码 ---

// RequestPasswordReset 发起找回密码。
//
// **邮箱不存在时也返回成功**。这一点是刻意的：如果返回"该邮箱未注册"，
// 这个接口就成了"某个人是不是本面板用户"的查询入口。代价是用户输错邮箱
// 时收不到任何提示——由"收不到邮件"承担这个反馈。
func (s *Service) RequestPasswordReset(ctx context.Context, email string, ci ClientInfo) error {
	if s.mailer == nil {
		return ErrMailNotConfigured
	}
	email, err := normalizeEmail(email)
	if err != nil {
		// 邮箱格式不合法同样返回成功：格式与"是否已注册"是两回事，
		// 但两者都不该给出可以区分的答案。
		log.Printf("[auth] 找回密码收到不合法邮箱: %v", err)
		return nil
	}
	if ok, err := s.mailer.Configured(ctx); err != nil || !ok {
		return ErrMailNotConfigured
	}

	user, err := s.userByEmail(ctx, email)
	if err != nil {
		return err
	}
	if user == nil {
		return nil
	}
	// 未激活 / 已封禁的账号不发码：给它发一封能改密码的邮件，
	// 等于让封禁失去意义。
	if !user.IsActive() {
		return nil
	}
	return s.issueCode(ctx, user, email, model.EmailCodeReset, resetCodeTTL, ci)
}

// VerifyResetCode 校验验证码并换发重置票据。
//
// 返回票据而不是直接改密码：邮件在传输途中可能被看到，而票据只出现在
// 这一次响应里，"看到邮件的人"与"能改密码的人"因此被分开。
func (s *Service) VerifyResetCode(ctx context.Context, email, code string, ci ClientInfo) (string, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return "", errInvalidCode
	}
	user, err := s.userByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	if user == nil || !user.IsActive() {
		return "", errInvalidCode
	}
	row, err := s.consumeCode(ctx, user, email, model.EmailCodeReset, code)
	if err != nil {
		return "", err
	}
	// 票据里带上验证码行的 ID（放在 sid 字段）。
	//
	// 没有它，票据就是一张在有效期内可以无限次重放的改密许可——邮件链接
	// 一旦被转发或留在浏览器历史里，谁点谁都能改密码。
	return s.tokens.Issue(strconv.FormatInt(row.ID, 10), user.ID, model.TokenTypeReset,
		time.Now().Add(resetTicketTTL))
}

// ResetPassword 用重置票据设置新密码。
//
// 改密后撤销该用户全部会话：找回密码的触发原因通常是"我怀疑账号被别人
// 用了"，留着旧会话会让这次重置只对"下次登录"生效。
func (s *Service) ResetPassword(ctx context.Context, ticket, newPassword string, ci ClientInfo) error {
	claims, err := s.tokens.Parse(ticket)
	if err != nil || claims.TokenType != model.TokenTypeReset {
		return api.Unauthenticated("重置链接已失效，请重新发起找回密码")
	}

	// 一次性：票据是无状态 JWT，签名有效就能重复提交，因此"用过没有"
	// 必须落到库里那一行验证码上。
	rowID, err := strconv.ParseInt(claims.SessionID, 10, 64)
	if err != nil {
		return errInvalidCode
	}
	var row model.EmailVerification
	if err := s.db.WithContext(ctx).Where("id = ? AND kind = ?", rowID, model.EmailCodeReset).
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInvalidCode
		}
		log.Printf("[auth] 查询验证码失败: %v", err)
		return api.Internal()
	}
	if row.ConsumedAt == nil || row.TicketUsedAt != nil {
		return errInvalidCode
	}

	var user model.User
	if err := s.db.WithContext(ctx).Where("id = ?", claims.UserID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errInvalidCode
		}
		log.Printf("[auth] 读取用户失败 id=%d: %v", claims.UserID, err)
		return api.Internal()
	}
	if !user.IsActive() {
		return errInvalidCode
	}
	if err := ValidateNewPassword(user.Username, newPassword); err != nil {
		return err
	}

	hash, err := HashPassword(newPassword)
	if err != nil {
		log.Printf("[auth] 生成密码哈希失败: %v", err)
		return api.Internal()
	}
	now := time.Now()
	// 票据的"已使用"与改密放在两个写操作里：先标记再改密。顺序反过来会
	// 出现"密码已改、票据仍可用"的窗口，而那个窗口里同一个链接还能再改
	// 一次——用户会以为自己已经把账号抢回来了。
	if err := s.db.WithContext(ctx).Model(&model.EmailVerification{}).
		Where("id = ?", row.ID).Update("ticket_used_at", now).Error; err != nil {
		log.Printf("[auth] 标记重置票据已使用失败: %v", err)
		return api.Internal()
	}
	if err := s.UpdateUser(ctx, user.ID, map[string]any{
		"password_hash":       hash,
		"security_updated_at": now,
	}); err != nil {
		return err
	}
	if _, err := s.RevokeOtherSessions(ctx, user.ID, 0); err != nil {
		return err
	}
	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID:   user.ID,
			OperatorName: user.Username,
			ResourceType: "user",
			ResourceID:   user.ID,
			ResourceName: user.Username,
			Action:       "user.password_reset",
			Success:      true,
			ClientIP:     ci.IP,
		})
	}
	return nil
}

// --- 内部实现 ---

// issueCode 生成并发送一个验证码。
func (s *Service) issueCode(
	ctx context.Context, user *model.User, email, kind string, ttl time.Duration, ci ClientInfo,
) error {
	row, err := s.latestCode(ctx, email, kind)
	if err != nil {
		return err
	}
	// 节流：同一个邮箱同一用途一分钟内只发一次。没有它，这个公开接口
	// 就是一台"往任意邮箱发信"的机器，而受害者会是邮箱的主人。
	if row != nil && time.Since(row.CreatedAt) < resendInterval && row.ConsumedAt == nil {
		return api.RateLimited("验证码已发送，请一分钟后重试")
	}

	code, err := randomCode(codeLength)
	if err != nil {
		return api.Internal()
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("[auth] 生成验证码哈希失败: %v", err)
		return api.Internal()
	}
	now := time.Now()
	rec := model.EmailVerification{
		UserID:    &user.ID,
		Email:     email,
		Kind:      kind,
		CodeHash:  string(hash),
		ExpiresAt: now.Add(ttl),
		ClientIP:  strPtr(ci.IP),
		CreatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&rec).Error; err != nil {
		log.Printf("[auth] 写入验证码失败: %v", err)
		return api.Internal()
	}

	scene := sceneBind
	if kind == model.EmailCodeReset {
		scene = sceneReset
	}
	if err := s.mailer.SendVerificationCode(ctx, email, scene, code); err != nil {
		// 发信失败时**删掉这条记录**：留着一个发不出去的码，表现为
		// "收到了旧邮件、输入却说不正确"，而用户根本不知道有过失败。
		_ = s.db.WithContext(ctx).Delete(&model.EmailVerification{}, rec.ID).Error
		log.Printf("[auth] 发送验证码邮件失败: %v", err)
		return api.Unavailable("邮件发送失败，请检查邮件配置")
	}
	return nil
}

// consumeCode 校验并消费一个验证码，返回被消费的那一行。
//
// 返回行是因为调用方（找回密码）要用它的 ID 给重置票据做一次性约束。
func (s *Service) consumeCode(
	ctx context.Context, user *model.User, email, kind, code string,
) (*model.EmailVerification, error) {
	row, err := s.latestCode(ctx, email, kind)
	if err != nil {
		return nil, err
	}
	if row == nil || row.UserID == nil || *row.UserID != user.ID {
		return nil, errInvalidCode
	}

	now := time.Now()
	if !row.Usable(now, maxCodeAttempts) {
		return nil, errInvalidCode
	}

	// 尝试次数先自增再校验：并发下两个请求可能都读到同一个 attempts，
	// 但自增是原子的 UPDATE，最终计数不会丢——少记一次等于多给一次机会。
	res := s.db.WithContext(ctx).Model(&model.EmailVerification{}).
		Where("id = ?", row.ID).Update("attempts", gorm.Expr("attempts + 1"))
	if res.Error != nil {
		log.Printf("[auth] 更新验证码尝试次数失败: %v", res.Error)
		return nil, api.Internal()
	}

	if err := bcrypt.CompareHashAndPassword([]byte(row.CodeHash), []byte(strings.TrimSpace(code))); err != nil {
		return nil, errInvalidCode
	}
	if err := s.db.WithContext(ctx).Model(&model.EmailVerification{}).
		Where("id = ?", row.ID).Update("consumed_at", now).Error; err != nil {
		log.Printf("[auth] 标记验证码已使用失败: %v", err)
		return nil, api.Internal()
	}
	return row, nil
}

// latestCode 取该邮箱该用途最近发出的验证码。
func (s *Service) latestCode(ctx context.Context, email, kind string) (*model.EmailVerification, error) {
	var row model.EmailVerification
	err := s.db.WithContext(ctx).
		Where("email = ? AND kind = ?", email, kind).
		Order("created_at DESC").First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil
	case err != nil:
		log.Printf("[auth] 查询验证码失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

// userByEmail 按已验证邮箱查找用户。
//
// 只认**已验证**的邮箱：用户在资料里填了但没确认过的地址不能用来找回
// 密码，否则任何人都能给自己填一个别人的邮箱去重置对方的账号。
func (s *Service) userByEmail(ctx context.Context, email string) (*model.User, error) {
	var user model.User
	err := s.db.WithContext(ctx).
		Where("lower(email) = ? AND email_verified_at IS NOT NULL", strings.ToLower(email)).
		Order("id ASC").First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, nil
	case err != nil:
		log.Printf("[auth] 按邮箱查询用户失败: %v", err)
		return nil, api.Internal()
	}
	return &user, nil
}

// normalizeEmail 做最小化的邮箱规范化：去空白、转小写。
//
// 不做格式强校验——正则永远有漏网的合法地址，而这里误判的代价是用户
// 收不到邮件，比"地址怪一点"严重得多。
func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") || strings.ContainsAny(email, "\r\n") {
		return "", api.InvalidParameter("邮箱地址不合法")
	}
	return email, nil
}

// randomCode 生成定长数字验证码。
func randomCode(n int) (string, error) {
	var b strings.Builder
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%d", v.Int64())
	}
	return b.String(), nil
}
