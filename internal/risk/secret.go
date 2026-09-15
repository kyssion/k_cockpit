package risk

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// 用途派生标签。
//
// 同一个根密钥派生多个用途的密钥，且**每个用途用不同标签**：直接复用根密钥
// 去做加密与签名，会让两者中的任一弱点波及另一处（例如签名侧的填充预言
// 影响加密侧）。
const (
	labelSign = "k_cockpit/risk/sign/v1"
	labelEnc  = "k_cockpit/risk/enc/v1"
)

var errSecretInvalid = errors.New("secret invalid")

// deriveKey 从根密钥派生指定用途的子密钥。
func deriveKey(root []byte, label string) []byte {
	mac := hmac.New(sha256.New, root)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// sealSecret 加密 TOTP 密钥。
//
// 数据库列名是 `totp_secret_enc`，即**约定加密存储**，不是可选项：TOTP
// 密钥等价于第二因素的全部凭据，明文入库意味着一次数据库泄漏（备份、只读
// 副本、SQL 注入）就等于所有用户的第二因素同时失效。
//
// 用 AES-GCM 而非单纯的 AES-CBC：GCM 自带完整性校验。缺少它的话，能写库的
// 攻击者可以把密钥改成自己已知的值，从而完全绕过第二因素——一个只保证
// 保密性、不保证完整性的方案在这里是不够的。
func sealSecret(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// nonce 前置到密文：解密时无需额外字段，且 nonce 本身不是秘密。
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// openSecret 解密 TOTP 密钥。
func openSecret(key []byte, encoded string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errSecretInvalid
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errSecretInvalid
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errSecretInvalid
	}
	return string(plaintext), nil
}
