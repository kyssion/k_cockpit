package risk

import (
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"k_cockpit/internal/model"
)

// tOTPPeriod 是动态码的时间步长。取值与常见验证器（Google Authenticator、
// 1Password）一致：30 秒，6 位数字，SHA-1。改动这里会让已验证用户全部失效。
const tOTPPeriod = 30 * time.Second

// tOTPSkew 是允许的时间偏移步数。
//
// 允许前后各 1 步（±30 秒）：不做容差会让时钟稍有偏差的用户无法验证，
// 而放宽到 2 步以上会让同一码的有效窗口超过 1 分钟，明显削弱其时效性。
const tOTPSkew = 1

// Method 是一种验证方式。
type Method string

// 支持的验证方式（R-007）。
const (
	MethodTOTP         Method = "totp"
	MethodRecoveryCode Method = "recovery_code"

	// MethodDevBypass 是**开发期万能验证码**，仅当配置了
	// SECURITY_DEV_BYPASS_CODE 时才出现在可用方式里。
	//
	// 它**不通过 valid() 检查**（见下）——这一点是刻意的：valid 决定
	// 「用户能否指定这种方式」，而开发模式追加它的路径在 Guard 内部，
	// 不经过这里。这样即便前端被改坏、硬塞了 `method=dev_bypass`，只要
	// 开发配置为空，请求仍然会被拒绝。
	MethodDevBypass Method = "dev_bypass"
)

// Label 返回验证方式的中文名，用于前端渲染。
func (m Method) Label() string {
	switch m {
	case MethodTOTP:
		return "验证器动态码"
	case MethodRecoveryCode:
		return "恢复码"
	case MethodDevBypass:
		return "开发万能码"
	default:
		return string(m)
	}
}

// valid 报告验证方式是否可由用户指定。
//
// MethodDevBypass **不在其中**：它只能由 Guard 在开发模式下主动追加，
// 不能被请求方直接指定（Guard.Verify 另有一处显式判断）。
func (m Method) valid() bool {
	switch m {
	case MethodTOTP, MethodRecoveryCode:
		return true
	default:
		return false
	}
}

// AvailableMethods 按用户**实际绑定情况**返回可用方式（R-007 / Q-005）。
//
// 必须由后端判定：用户可能只绑了 TOTP、或用完了恢复码。前端硬编码三种方式
// 会在这些场景下弹出无法完成的验证框，把用户卡死在一个退不出去的弹窗里。
func AvailableMethods(u *model.User) []Method {
	if u == nil {
		return nil
	}

	methods := make([]Method, 0, 2)
	if u.TotpEnabled && u.TotpSecretEnc != nil && *u.TotpSecretEnc != "" {
		methods = append(methods, MethodTOTP)
	}
	if u.RecoveryCodesHash != nil && *u.RecoveryCodesHash != "" {
		methods = append(methods, MethodRecoveryCode)
	}
	return methods
}

// Verification 是一次验证的结果。
type Verification struct {
	// OK 表示验证是否通过。
	//
	// **不区分失败原因**（R-009）：码错误与码过期返回同一个结果，调用方也
	// 只能给出一段统一文案。区分会帮攻击者判断自己的猜测是否接近（「过期」
	// 说明格式对了、只是慢了；「错误」则说明还早得很）。
	OK bool

	// RemainingRecoveryCodes 仅在使用恢复码且通过时有意义（R-008）。
	RemainingRecoveryCodes int
	// UpdatedRecoveryHash 是消费掉一个恢复码后的新哈希串，需由调用方写回
	// 用户记录。返回给调用方而不是在本包内写库：本包不依赖数据库，
	// 才能被单独测试。
	UpdatedRecoveryHash string
}

// Verify 校验一次验证码。
//
// tOTPSecret 传入的是**已解密**的 TOTP 密钥，而不是库中存储的密文：解密
// 由调用方（持有密钥的 Guard）负责，本函数不接触密钥的存储形式，因而可以
// 单独测试。
//
// 把密文传进来的写法能通过编译、也能运行，只是永远校验失败——用户会看到
// 「验证码不正确」，而问题其实出在密钥根本没被解密。
func Verify(u *model.User, tOTPSecret string, method Method, code string, now time.Time) Verification {
	if u == nil || !method.valid() {
		return Verification{}
	}

	switch method {
	case MethodTOTP:
		if !u.TotpEnabled || tOTPSecret == "" {
			return Verification{}
		}
		return Verification{OK: validateTOTP(tOTPSecret, code, now)}

	case MethodRecoveryCode:
		if u.RecoveryCodesHash == nil || *u.RecoveryCodesHash == "" {
			return Verification{}
		}
		remaining, updated, ok := consumeRecoveryCode(*u.RecoveryCodesHash, code)
		if !ok {
			return Verification{}
		}
		return Verification{
			OK:                     true,
			RemainingRecoveryCodes: remaining,
			UpdatedRecoveryHash:    updated,
		}
	}
	return Verification{}
}

// validateTOTP 校验动态码。
func validateTOTP(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}

	ok, err := totp.ValidateCustom(code, secret, now, totp.ValidateOpts{
		Period:    uint(tOTPPeriod.Seconds()),
		Skew:      tOTPSkew,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return err == nil && ok
}
