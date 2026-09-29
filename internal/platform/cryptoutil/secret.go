// Package cryptoutil 提供可逆加密的存储封装。
//
// 本项目有几处需要**可逆**保存的敏感值：TOTP 密钥、VNC 密码、来宾凭据。
// 它们的共同点是——控制面必须在某个时刻把明文交给 agent（或参与校验），
// 因此不能用单向哈希。
//
// 用 AES-GCM 而非单纯的 AES-CBC：GCM 自带完整性校验。缺少它的话，能写库的
// 攻击者可以把密钥改成自己已知的值，从而完全绕过第二因素；对 VNC 密码而言
// 则是把密码换成他控制的那个，然后在用户不知情时接入控制台。只保证保密性、
// 不保证完整性的方案在这些场景下都不够。
package cryptoutil

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

// ErrInvalid 表示密文不可解密（格式错误、被篡改或密钥不符）。
//
// 对外一律映射为同一段文案：区分「格式错」与「密钥不符」会给探测者提供
// 判断依据，而调用方对这两者的处理也完全一样——都是「这个值不可用」。
var ErrInvalid = errors.New("cryptoutil: invalid ciphertext")

// DeriveKey 从根密钥派生指定用途的子密钥。
//
// 每个用途用**不同标签**：直接复用根密钥去做加密与签名，会让两者中任一
// 处的弱点波及另一处。
func DeriveKey(root []byte, label string) []byte {
	mac := hmac.New(sha256.New, root)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// Seal 加密明文，返回可直接入库的字符串。
//
// nonce 前置到密文：解密时无需额外字段，且 nonce 本身不是秘密。
func Seal(key []byte, plaintext string) (string, error) {
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

	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open 解密由 Seal 产生的字符串。
func Open(key []byte, encoded string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", ErrInvalid
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
		return "", ErrInvalid
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrInvalid
	}
	return string(plaintext), nil
}
