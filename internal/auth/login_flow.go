package auth

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// Stage 是登录后所处的阶段。
const (
	// StageOK 已完成，令牌是访问级别。
	StageOK Stage = "ok"
	// StageVerify 等待二次验证（TOTP 或恢复码）。
	StageVerify Stage = "login_verify"
	// StageForceChange 强制改密：账号由管理员或应急流程创建，初始密码
	// 不属于使用者本人。
	StageForceChange Stage = "force_password_change"
	// StageBootstrap 安全初始化引导：管理员首次登录时补齐邮箱与 2FA。
	StageBootstrap Stage = "bootstrap_security"
)

// Stage 是登录阶段。
type Stage string

// stageTTL 是登录中间态会话的寿命。
//
// 取 5 分钟而不是复用空闲超时：这一段会话的目的只是"让用户把验证码输完"，
// 给它两个小时等于在两个小时里保留一枚只差一步就能换到完整权限的令牌。
const stageTTL = 5 * time.Minute

// ErrStageMismatch 表示令牌级别与接口要求不符。
var ErrStageMismatch = api.Unauthenticated("登录状态已失效，请重新登录")

// errInvalidCode 是验证码失败的统一错误。
//
// 不区分「码错了」「码过期了」「次数用完了」：区分会让攻击者据此判断
// 自己的尝试离成功有多近（risk R-009 同一条理由）。
var errInvalidCode = api.Unauthenticated("验证码不正确或已失效")

// LoginFactorVerifier 校验登录阶段的二次验证码。
//
// 声明为接口是因为真正的校验在 internal/risk：TOTP 密钥的解密、恢复码的
// 一次性消费都在那里，而 risk 反过来依赖本包。重复实现一份会让"在安全
// 中心能用、登录时却不行"成为一类查不出原因的问题。
type LoginFactorVerifier interface {
	VerifyLoginCode(ctx context.Context, user *model.User, code string) (bool, error)
}

// SetLoginVerifier 注入登录阶段的验证码校验器。
//
// 用 setter 而不是构造函数参数：现有的调用点（包括大量测试）构造服务时
// 并不关心二次验证，为一个可选能力改动全部签名不值得。
func (s *Service) SetLoginVerifier(v LoginFactorVerifier) { s.verifier = v }

// NeedsBootstrap 报告用户是否必须完成安全初始化。
//
// 只对**管理员**要求：普通用户的账号由管理员分配，要求他也绑邮箱与 2FA
// 会把"管理员批量开通十个账号"变成十次无法跳过的引导。邮箱与 2FA 对
// 管理员是必要的——他能接管整台宿主机。
//
// BootstrapSkipped 一旦为真就不再提示：引导可以跳过，但**不能被遗忘打扰
// 一辈子**；跳过后仍可在安全中心自行补齐。
func NeedsBootstrap(u *model.User) bool {
	if u == nil || !u.IsAdmin() || u.BootstrapSkipped {
		return false
	}
	return u.EmailVerifiedAt == nil || !u.TotpEnabled
}

// startFlow 决定登录后进入哪个阶段并签发对应级别的会话。
func (s *Service) startFlow(ctx context.Context, user *model.User, ci ClientInfo) (*LoginResult, error) {
	stage := stageOfStaged(user)
	if stage == StageOK {
		return s.issueSession(ctx, user, model.TokenTypeAccess, 0, ci, StageOK)
	}
	return s.issueSession(ctx, user, model.TokenTypeLogin, stageTTL, ci, stage)
}

// stageOfStaged 判定一个**尚未完成登录**的用户停在哪个阶段。
//
// 顺序即优先级：强制改密优先于二次验证——账号的初始密码是别人设的，此时
// 要求验证器动态码，等于要求使用者先证明"我是这个还不完全属于我的账号的
// 主人"。
func stageOfStaged(user *model.User) Stage {
	switch {
	case user.ForcePasswordChange:
		return StageForceChange
	case user.TotpEnabled:
		return StageVerify
	case NeedsBootstrap(user):
		return StageBootstrap
	default:
		return StageOK
	}
}

// AuthenticateStage 校验登录中间态令牌。
//
// 复用 Authenticate 的全部判定（签名、会话撤销、指纹、账号状态、安全信息
// 变更），只额外要求令牌级别相符——中间态令牌不能用来换访问会话，访问
// 令牌也不能用来走未完成的登录流程。
//
// 传 Passive：中间态会话的寿命是固定的五分钟，按真实活动续期会把它拖成
// 一个长期有效的半权限令牌。
func (s *Service) AuthenticateStage(
	ctx context.Context, token, wantType string, ci ClientInfo,
) (*model.User, *model.Session, error) {
	user, session, err := s.Authenticate(ctx, token, ci, Passive)
	if err != nil {
		return nil, nil, err
	}
	if session.TokenType != wantType {
		return nil, nil, ErrStageMismatch
	}
	return user, session, nil
}

// VerifyLogin 完成登录阶段的二次验证，换发访问会话。
func (s *Service) VerifyLogin(
	ctx context.Context, token, code string, ci ClientInfo,
) (*LoginResult, error) {
	user, session, err := s.AuthenticateStage(ctx, token, model.TokenTypeLogin, ci)
	if err != nil {
		return nil, err
	}
	// 只有真的处于"等验证码"这一步才接受：否则一个刚被强制改密的用户
	// 可以拿 TOTP 绕开改密。
	if stageOfStaged(user) != StageVerify {
		return nil, ErrStageMismatch
	}
	if s.verifier == nil {
		logStage("未注入登录验证码校验器")
		return nil, api.Unavailable("二次验证服务暂时不可用")
	}

	ok, err := s.verifier.VerifyLoginCode(ctx, user, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errInvalidCode
	}

	// 中间态会话一次性：换到访问会话后立刻撤销，避免它继续被调用。
	if err := s.revokeSessionRow(ctx, session.ID, user.ID); err != nil {
		return nil, err
	}
	return s.issueSession(ctx, user, model.TokenTypeAccess, 0, ci, StageOK)
}

// ForceChangePassword 完成强制改密并进入系统。
//
// 改密后撤销该用户其它会话：初始密码很可能已经（至少在流程上）被第三人
// 知道，留着旧会话等于改密只改了一半。
func (s *Service) ForceChangePassword(
	ctx context.Context, token, newPassword string, ci ClientInfo,
) (*LoginResult, error) {
	user, session, err := s.AuthenticateStage(ctx, token, model.TokenTypeLogin, ci)
	if err != nil {
		return nil, err
	}
	if !user.ForcePasswordChange {
		return nil, ErrStageMismatch
	}
	if err := ValidateNewPassword(user.Username, newPassword); err != nil {
		return nil, err
	}

	hash, err := HashPassword(newPassword)
	if err != nil {
		logStage("生成密码哈希失败")
		return nil, api.Internal()
	}
	now := time.Now()
	if err := s.UpdateUser(ctx, user.ID, map[string]any{
		"password_hash":         hash,
		"force_password_change": false,
		// 让此前签发的全部会话失效（R-010）：改密是最典型的"我怀疑
		// 账号已被他人使用"场景。
		"security_updated_at": now,
	}); err != nil {
		return nil, err
	}
	user.PasswordHash = hash
	user.ForcePasswordChange = false
	user.SecurityUpdatedAt = &now

	if err := s.revokeSessionRow(ctx, session.ID, user.ID); err != nil {
		return nil, err
	}
	// 当前中间态会话被撤销后，其余会话已因 security_updated_at 失效，
	// 无需再单独撤销。
	return s.issueSession(ctx, user, model.TokenTypeAccess, 0, ci, StageOK)
}

// CompleteBootstrap 清除"已跳过安全初始化"的标记。
//
// 走**访问级**会话而不是登录中间态：补齐邮箱与两步验证是在安全中心做的，
// 那时用户早已登录完成。引导期本身不需要这一步——条件齐了之后下次登录
// 自然就是 ok，不会停在 bootstrap_security。
//
// 这个标记清掉才有意义：它唯一的后果是"下次登录还提不提醒"，留着它会
// 让人以为自己一直处于未做安全设置的状态。
func (s *Service) CompleteBootstrap(ctx context.Context, user *model.User) error {
	if !user.BootstrapSkipped {
		return nil
	}
	if user.EmailVerifiedAt == nil || !user.TotpEnabled {
		return api.ValidationFailed("请先绑定邮箱并启用两步验证")
	}
	if err := s.UpdateUser(ctx, user.ID, map[string]any{"bootstrap_skipped": false}); err != nil {
		return err
	}
	user.BootstrapSkipped = false
	return nil
}

// SkipBootstrap 跳过安全初始化引导。
//
// 允许跳过是刻意的：一个自建自用的面板里，管理员可能根本没有可用邮箱。
// 此时如果不给出口，他唯一的选择是去改数据库。
func (s *Service) SkipBootstrap(
	ctx context.Context, token string, ci ClientInfo,
) (*LoginResult, error) {
	user, session, err := s.AuthenticateStage(ctx, token, model.TokenTypeLogin, ci)
	if err != nil {
		return nil, err
	}
	if err := s.UpdateUser(ctx, user.ID, map[string]any{"bootstrap_skipped": true}); err != nil {
		return nil, err
	}
	user.BootstrapSkipped = true

	if err := s.revokeSessionRow(ctx, session.ID, user.ID); err != nil {
		return nil, err
	}
	return s.issueSession(ctx, user, model.TokenTypeAccess, 0, ci, StageOK)
}

// revokeSessionRow 撤销指定会话（按行 ID），用于中间态会话的一次性消费。
func (s *Service) revokeSessionRow(ctx context.Context, rowID, userID int64) error {
	res := s.db.WithContext(ctx).Model(&model.Session{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", rowID, userID).
		Update("revoked_at", time.Now())
	if res.Error != nil {
		logStage("撤销登录中间态会话失败")
		return api.Internal()
	}
	return nil
}

// logStage 记录本文件内的异常，统一前缀便于检索。
func logStage(msg string) { log.Print("[auth:stage] " + msg) }
