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

	if !validateTOTP(secret, code, time.Now()) {
		// 这里的文案可以指出「码不对」：绑定流程不涉及凭据猜测，用户
		// 就在现场看着验证器，含糊其辞只会让他反复重试。
		return nil, api.ValidationFailed("验证码不正确，请确认验证器时间准确后重试")
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
}

// SetupState 返回当前用户的绑定进度。
func SetupState(user *model.User) SetupInfo {
	state := SetupInfo{TOTPEnabled: user.TotpEnabled}
	if user.TotpSecretEnc != nil && *user.TotpSecretEnc != "" && !user.TotpEnabled {
		state.PendingSetup = true
	}
	if user.RecoveryCodesHash != nil && *user.RecoveryCodesHash != "" {
		state.RecoveryCodesOK = true
		state.RecoveryCodes = len(splitHashes(*user.RecoveryCodesHash))
	}
	return state
}

// NormalizeCode 归一化用户输入的验证码。
//
// 前端可能把动态码按「123 456」分组提交，去掉空格再校验，避免用户
// 「明明输对了却说错误」——这类问题排查起来极其费时。
func NormalizeCode(code string) string {
	return strings.TrimSpace(code)
}
