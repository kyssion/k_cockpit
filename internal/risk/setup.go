package risk

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// issuerName 是验证器 App 中显示的签发方名称。
const issuerName = "K Cockpit"

// BeginTOTPSetup 生成一个新的 TOTP 密钥并写入用户记录，但**不启用**。
//
// 分两步（生成 → 确认）而不是一步到位：用户可能扫错码、或验证器时区不对，
// 一步启用会让他在下次登录时才发现自己进不去。确认步骤要求提交一次真实
// 动态码，确保「用户确实已经能算出码」之后才启用。
func (g *Guard) BeginTOTPSetup(ctx context.Context, user *model.User) (string, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuerName,
		AccountName: user.Username,
		Period:      uint(tOTPPeriod.Seconds()),
		SecretSize:  20, // 160 bit，与 RFC 4226 对 HMAC-SHA1 的建议一致
		Digits:      otp.DigitsSix,
		Algorithm:   otp.AlgorithmSHA1,
	})
	if err != nil {
		log.Printf("[risk] 生成 TOTP 密钥失败: %v", err)
		return "", api.Internal()
	}

	encrypted, err := sealSecret(g.encKey, key.Secret())
	if err != nil {
		log.Printf("[risk] 加密 TOTP 密钥失败: %v", err)
		return "", api.Internal()
	}

	// 重置为未启用：重新绑定时旧密钥必须立即失效，否则「解绑再绑定」
	// 会让两个密钥同时可用，而用户以为旧的已经作废了。
	err = g.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", user.ID).
		Updates(map[string]any{
			"totp_secret_enc": encrypted,
			"totp_enabled":    false,
		}).Error
	if err != nil {
		log.Printf("[risk] 保存 TOTP 密钥失败 user=%d: %v", user.ID, err)
		return "", api.Internal()
	}

	// 返回 otpauth:// URI，由前端渲染成二维码。**不返回二维码图片**：
	// 那是展示层的事，服务端生成图片只会多一层依赖。
	return key.URL(), nil
}

// ConfirmTOTPSetup 校验一次动态码并启用 TOTP，同时生成恢复码。
//
// 恢复码在**启用成功时一并生成**，且明文只在此刻返回一次（R-008 的前置
// 条件：没有恢复码，用户丢了手机就彻底进不去）。
func (g *Guard) ConfirmTOTPSetup(
	ctx context.Context, user *model.User, code string,
) ([]string, error) {
	if user.TotpSecretEnc == nil || *user.TotpSecretEnc == "" {
		return nil, api.ValidationFailed("请先开始绑定流程")
	}

	secret, err := openSecret(g.encKey, *user.TotpSecretEnc)
	if err != nil {
		log.Printf("[risk] 解密 TOTP 密钥失败 user=%d: %v", user.ID, err)
		return nil, api.Internal()
	}

	// 开发期万能码同样作用于绑定确认，而且**必须**在这里也生效：绑定是
	// 整个二次验证的前置条件，卡在这一步等于所有高风险操作都做不了。
	//
	// 需要说明的是：此码通过时并没有验证「用户真的能算出动态码」——而这
	// 正是确认步骤本来的目的。因此密钥依然保存，但该账号的 TOTP 是否可用
	// 是未经验证的。
	devBypass := g.isDevBypass(code)
	if !devBypass && !validateTOTP(secret, code, time.Now()) {
		// 这里的文案可以指出「码不对」：绑定流程不涉及凭据猜测，用户
		// 就在现场看着验证器，含糊其辞只会让他反复重试。
		return nil, api.ValidationFailed("验证码不正确，请确认验证器时间准确后重试")
	}
	if devBypass {
		log.Printf("[risk] ⚠ 开发期万能码通过了 TOTP 绑定确认 user=%d —— "+
			"未验证该账号能否真的算出动态码", user.ID)
	}

	codes, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		log.Printf("[risk] 生成恢复码失败: %v", err)
		return nil, api.Internal()
	}

	now := time.Now()
	err = g.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", user.ID).
		Updates(map[string]any{
			"totp_enabled":        true,
			"recovery_codes_hash": hashes,
			// 安全信息变更时间：会话层据此判定「此前签发的令牌是否仍可信」，
			// 也是审计追溯「这次变更发生在什么时候」的依据。
			"security_updated_at": now,
		}).Error
	if err != nil {
		log.Printf("[risk] 启用 TOTP 失败 user=%d: %v", user.ID, err)
		return nil, api.Internal()
	}

	return codes, nil
}

// SetupInfo 报告用户的绑定进度，供前端决定展示哪一步。
type SetupInfo struct {
	TOTPEnabled     bool `json:"totp_enabled"`
	PendingSetup    bool `json:"pending_setup"`
	RecoveryCodes   int  `json:"recovery_code_count"`
	RecoveryCodesOK bool `json:"has_recovery_codes"`
	// DevBypass 表示开发期万能码当前是否可用。
	//
	// 显式回报给前端，是为了让「当前防护处于什么状态」**看得见**——一个
	// 只在环境变量里的开关，很容易在某次部署中被忘记，而界面上一直显示
	// 「已绑定」会让所有人以为防护是完整的。
	DevBypass bool `json:"dev_bypass"`

	// Email 与 EmailVerified 供安全中心展示邮箱绑定状态。
	//
	// 邮箱不是二次验证的一种方式，但没有它就没有找回密码这条路——把它
	// 列在这里，是因为用户判断「我能从事故里恢复吗」时，这两件事是连着
	// 看的：验证器丢了、邮箱也没绑，就真的进不去了。
	Email         string `json:"email,omitempty"`
	EmailVerified bool   `json:"email_verified"`
	// BootstrapSkipped 表示管理员曾跳过安全初始化引导。
	BootstrapSkipped bool `json:"bootstrap_skipped"`
}

// SetupState 返回当前用户的绑定进度。
//
// devBypass 由调用方从 Guard 取：本包内 Guard 才是配置的持有者，而这
// 个函数是纯函数（不依赖 Guard），便于单独测试。
func SetupState(user *model.User, devBypass bool) SetupInfo {
	state := SetupInfo{TOTPEnabled: user.TotpEnabled, DevBypass: devBypass}
	if user.TotpSecretEnc != nil && *user.TotpSecretEnc != "" && !user.TotpEnabled {
		state.PendingSetup = true
	}
	if user.RecoveryCodesHash != nil && *user.RecoveryCodesHash != "" {
		state.RecoveryCodesOK = true
		state.RecoveryCodes = len(splitHashes(*user.RecoveryCodesHash))
	}
	if user.Email != nil {
		state.Email = *user.Email
	}
	state.EmailVerified = user.EmailVerifiedAt != nil
	state.BootstrapSkipped = user.BootstrapSkipped
	return state
}

// NormalizeCode 归一化用户输入的验证码。
//
// 前端可能把动态码按「123 456」分组提交，去掉空格再校验，避免用户
// 「明明输对了却说错误」——这类问题排查起来极其费时。
func NormalizeCode(code string) string {
	return strings.TrimSpace(code)
}

// RevokeUserGrants 丢弃该用户全部未消费的一次性许可，返回丢弃数量。
func (g *Guard) RevokeUserGrants(userID int64) int {
	return g.grants.RevokeUser(userID)
}

// RegenerateRecoveryCodes 生成一批新的恢复码并**作废旧的全部**（F-10-01）。
//
// 与 ConfirmTOTPSetup 里那一段是同一个动作（生成 + 写回 + 作废旧许可），
// 区别只在于不重新生成 TOTP 密钥——用户是在已有的验证器上补充恢复码。
//
// **旧码必须一起作废**：发一批新的"万能钥匙"而旧的还能用，等于把可用的
// 入口翻了一倍，而用户以为只换了新的那一张纸——抽屉里那张旧的依然能进来。
func (g *Guard) RegenerateRecoveryCodes(ctx context.Context, user *model.User) ([]string, error) {
	codes, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		log.Printf("[risk] 生成恢复码失败: %v", err)
		return nil, api.Internal()
	}

	now := time.Now()
	if err := g.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", user.ID).
		Updates(map[string]any{
			"recovery_codes_hash": hashes,
			"security_updated_at": now,
		}).Error; err != nil {
		log.Printf("[risk] 更新恢复码失败: %v", err)
		return nil, api.Internal()
	}

	// 此前签发的一次性许可作废：它们是在旧凭据下换来的。
	g.grants.RevokeUser(user.ID)
	return codes, nil
}
