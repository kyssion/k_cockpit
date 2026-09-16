package risk

import "k_cockpit/internal/cryptoutil"

// 用途派生标签。
const (
	labelSign = "k_cockpit/risk/sign/v1"
	labelEnc  = "k_cockpit/risk/enc/v1"
)

// deriveKey 从根密钥派生指定用途的子密钥。
func deriveKey(root []byte, label string) []byte {
	return cryptoutil.DeriveKey(root, label)
}

// errSecretInvalid 在不可解密时返回。
//
// 直接复用公共包的错误值而不另建一个：调用方对两者的处理完全相同
// （「这个值不可用」），多一层包装只会让错误链更难读。
var errSecretInvalid = cryptoutil.ErrInvalid

// sealSecret 加密 TOTP 密钥。
//
// 数据库列名是 `totp_secret_enc`，即**约定加密存储**，不是可选项：TOTP
// 密钥等价于第二因素的全部凭据，明文入库意味着一次数据库泄漏（备份、
// 只读副本、SQL 注入）就等于所有用户的第二因素同时失效。
func sealSecret(key []byte, plaintext string) (string, error) {
	return cryptoutil.Seal(key, plaintext)
}

// openSecret 解密 TOTP 密钥。
func openSecret(key []byte, encoded string) (string, error) {
	return cryptoutil.Open(key, encoded)
}
