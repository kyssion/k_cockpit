package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"k_cockpit/internal/api"
)

// Argon2id 参数。取值参考 OWASP 建议（19 MiB 内存、2 次迭代、单并行度），
// 在抗暴力破解与登录耗时之间取平衡。
const (
	argonMemory      = 19 * 1024 // KiB
	argonIterations  = 2
	argonParallelism = 1
	argonSaltLen     = 16
	argonKeyLen      = 32
)

// ErrInvalidHash 表示存储的密码哈希格式无法解析。
//
// 它的语义是「数据损坏或被人为篡改」，**不是**密码错误：密码不匹配时
// VerifyPassword 返回 (false, nil)。
var ErrInvalidHash = errors.New("密码哈希格式无效")

// HashPassword 使用 Argon2id 生成密码哈希。
//
// 返回自描述的 PHC 字符串（含算法、版本与参数），因此将来调整参数时
// **旧密码仍可正常校验**（f-1-01 R-001）。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt,
		argonIterations, argonMemory, argonParallelism, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// ValidateNewPassword 校验新密码是否可接受。
//
// 只检查**能被客观判定的**两条：长度下限与不得包含用户名。不做字符种类
// 打分——那种规则的实际效果是让用户把首字母大写再加个 1，而它带来的摩擦
// 是真实的。
func ValidateNewPassword(username, password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return api.InvalidParameter("密码长度不得少于 12 个字符")
	}
	if username != "" && strings.Contains(
		strings.ToLower(password), strings.ToLower(username),
	) {
		return api.InvalidParameter("密码不能包含用户名")
	}
	return nil
}

// VerifyPassword 校验明文密码与存储的哈希是否匹配。
//
// 比较使用常量时间实现，避免通过响应时间差推断哈希内容。
// 哈希格式非法时返回 ErrInvalidHash，而不是静默判定为不匹配。
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt,
		params.iterations, params.memory, params.parallelism, uint32(len(want)))

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// argonParams 是从存储的哈希串中解析出的参数。
type argonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

// decodeHash 解析 PHC 格式的 Argon2id 哈希串：
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// 参数从串中读取而非使用全局常量，保证参数调整后旧密码依然可校验。
func decodeHash(encoded string) (*argonParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, nil, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return nil, nil, nil, ErrInvalidHash
	}

	params := &argonParams{}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&params.memory, &params.iterations, &params.parallelism); err != nil {
		return nil, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, nil, ErrInvalidHash
	}
	if len(salt) == 0 || len(key) == 0 {
		return nil, nil, nil, ErrInvalidHash
	}

	return params, salt, key, nil
}
