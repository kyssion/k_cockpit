package risk

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// RecoveryCodeCount 是每次生成的恢复码数量。
//
// 10 个是常见取值：足够应对多次「手机丢了」，又少到用户会认真保管。
// 数量太多会让用户随手丢在邮件里，反而降低安全性。
const RecoveryCodeCount = 10

// recoveryAlphabet 是恢复码使用的字符集。
//
// 刻意去掉易混淆字符（0/O、1/I/L）：恢复码是要用户**手抄和手输**的，
// 把 0 和 O 放在一起，等于制造一个必然发生的输错。
const recoveryAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// recoveryCodeLength 是每个恢复码的长度。
const recoveryCodeLength = 12

// GenerateRecoveryCodes 生成一批恢复码。
//
// 返回明文（只在此刻返回一次，供用户保存）与可直接入库的哈希串。
func GenerateRecoveryCodes() ([]string, string, error) {
	plain := make([]string, 0, RecoveryCodeCount)
	hashes := make([]string, 0, RecoveryCodeCount)

	for i := 0; i < RecoveryCodeCount; i++ {
		code, err := randomRecoveryCode()
		if err != nil {
			return nil, "", err
		}
		plain = append(plain, code)
		hashes = append(hashes, hashRecoveryCode(code))
	}

	return plain, strings.Join(hashes, "\n"), nil
}

// consumeRecoveryCode 校验并**消费**一个恢复码。
//
// 返回剩余数量与更新后的哈希串。消费即失效（R-008）：恢复码本质是一次性
// 凭据，不失效等于留下一个永久后门——一旦泄漏就长期有效。
func consumeRecoveryCode(stored, code string) (remaining int, updated string, ok bool) {
	normalized := normalizeRecoveryCode(code)
	if normalized == "" {
		return 0, "", false
	}

	hashes := splitHashes(stored)
	want := hashRecoveryCode(normalized)

	for i, h := range hashes {
		// 常量时间比较：用 == 逐字节短路会在耗时上泄漏「前几个字符猜对了」。
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			rest := append(append([]string{}, hashes[:i]...), hashes[i+1:]...)
			return len(rest), strings.Join(rest, "\n"), true
		}
	}
	return len(hashes), "", false
}

// hashRecoveryCode 计算恢复码的存储哈希。
//
// 内部先做归一化，**调用方不必也不应自己处理**：生成与校验两处若处理不一致
// （一处带连字符、一处去掉），结果就是「生成的码永远校验不通过」——而在用户
// 看来这像是「我输错了」，没人会去怀疑是服务端两处实现不一致。
//
// 用 SHA-256 而非 Argon2id：恢复码是**高熵随机串**（12 位、31 个字符的
// 字母表，约 59 bit），暴力破解不可行，慢哈希在这里没有意义；代价却很实在
// ——每次验证要遍历比对 10 个码，用 Argon2id 会让这个过程慢上几个数量级。
// 慢哈希解决的是「人选的低熵密码」，恢复码不属于这一类。
func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeRecoveryCode(code)))
	return hex.EncodeToString(sum[:])
}

// normalizeRecoveryCode 归一化用户输入。
//
// 用户会按自己习惯加空格或连字符来分组（甚至全角空格），把这些差异一律
// 抹平，避免「明明输对了却说错误」——这类问题排查起来极其费时。
func normalizeRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case ' ', '\t', '-', '_', '\u3000':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// splitHashes 拆分存储的哈希串，忽略空行。
func splitHashes(stored string) []string {
	lines := strings.Split(stored, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// randomRecoveryCode 生成一个随机恢复码（形如 XXXX-XXXX-XXXX）。
func randomRecoveryCode() (string, error) {
	buf := make([]byte, recoveryCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	out := make([]byte, 0, recoveryCodeLength+2)
	for i, b := range buf {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, recoveryAlphabet[int(b)%len(recoveryAlphabet)])
	}
	return string(out), nil
}
