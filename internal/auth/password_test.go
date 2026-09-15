package auth_test

import (
	"strings"
	"testing"

	"k_cockpit/internal/auth"
)

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"

	encoded, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}

	ok, err := auth.VerifyPassword(encoded, password)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !ok {
		t.Error("正确密码未通过校验")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	encoded, err := auth.HashPassword("right-password")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}

	ok, err := auth.VerifyPassword(encoded, "wrong-password")
	if err != nil {
		t.Fatalf("校验不应返回错误: %v", err)
	}
	if ok {
		t.Error("错误密码通过了校验")
	}
}

// 同一密码两次哈希必须不同——否则等于没加盐，彩虹表可直接命中。
func TestHashIsSalted(t *testing.T) {
	a, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	b, err := auth.HashPassword("same-password")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	if a == b {
		t.Error("两次哈希结果相同，说明盐未随机")
	}
}

// 哈希串必须自描述：含算法与参数，将来调整参数时旧密码才仍可校验（R-001）。
func TestHashIsSelfDescribing(t *testing.T) {
	encoded, err := auth.HashPassword("pw")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}

	for _, want := range []string{"$argon2id$", "v=", "m=", "t=", "p="} {
		if !strings.Contains(encoded, want) {
			t.Errorf("哈希串缺少 %q: %s", want, encoded)
		}
	}
	if strings.Contains(encoded, "pw") {
		t.Error("哈希串中出现了明文密码")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	cases := map[string]string{
		"空串":         "",
		"段数不足":       "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
		"算法不符":       "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"版本不符":       "$argon2id$v=13$m=19456,t=2,p=1$c2FsdA$aGFzaA",
		"参数非法":       "$argon2id$v=19$m=abc,t=2,p=1$c2FsdA$aGFzaA",
		"盐非法 base64": "$argon2id$v=19$m=19456,t=2,p=1$!!!$aGFzaA",
		"哈希为空":       "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$",
	}

	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := auth.VerifyPassword(encoded, "pw")
			if err == nil {
				t.Error("非法哈希应返回错误，而不是判定为不匹配")
			}
			if ok {
				t.Error("非法哈希不应校验通过")
			}
		})
	}
}

// 空密码也应能被正常哈希与校验（是否允许空密码是上层策略，不是哈希层职责）。
func TestEmptyPassword(t *testing.T) {
	encoded, err := auth.HashPassword("")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}

	ok, err := auth.VerifyPassword(encoded, "")
	if err != nil || !ok {
		t.Errorf("空密码校验失败: ok=%v err=%v", ok, err)
	}

	ok, err = auth.VerifyPassword(encoded, "x")
	if err != nil || ok {
		t.Errorf("空密码不应匹配非空输入: ok=%v err=%v", ok, err)
	}
}
