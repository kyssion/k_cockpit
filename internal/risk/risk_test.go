package risk

import (
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/model"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	return deriveKey([]byte("test-root-secret-for-risk-package"), labelEnc)
}

// --- 许可 ---

func TestGrantIsSingleUse(t *testing.T) {
	store := NewStore()
	now := time.Now()

	g, err := store.Issue(1, 10, MethodTOTP, now)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	if got := store.Consume(g.Token, 1, 10, now); got == nil {
		t.Fatal("首次消费应通过")
	}
	// 一次性是整个机制的核心：许可能重复使用，「验证一次、连续删多次」
	// 就会成为现实，二次确认的意义随之消失（Q-004）。
	if got := store.Consume(g.Token, 1, 10, now); got != nil {
		t.Error("同一许可被重复消费，一次性保证失效")
	}
}

func TestGrantExpires(t *testing.T) {
	store := NewStore()
	now := time.Now()

	g, _ := store.Issue(1, 10, MethodTOTP, now)
	later := now.Add(GrantTTL + time.Second)

	if got := store.Consume(g.Token, 1, 10, later); got != nil {
		t.Error("过期许可仍被接受")
	}
}

func TestGrantBoundToSession(t *testing.T) {
	store := NewStore()
	now := time.Now()

	g, _ := store.Issue(1, 10, MethodTOTP, now)

	// 同一用户的另一个会话：仅比对用户是不够的，那等于把「当前会话已验证」
	// 偷换成「这个人曾验证过」（R-005）。
	if got := store.Consume(g.Token, 1, 999, now); got != nil {
		t.Error("许可可跨会话使用")
	}
	if got := store.Consume(g.Token, 2, 10, now); got != nil {
		t.Error("许可可跨用户使用")
	}
}

func TestGrantRevokeSession(t *testing.T) {
	store := NewStore()
	now := time.Now()

	g1, _ := store.Issue(1, 10, MethodTOTP, now)
	g2, _ := store.Issue(1, 20, MethodTOTP, now)

	if removed := store.RevokeSession(10); removed != 1 {
		t.Errorf("撤销数量 = %d, 期望 1", removed)
	}
	if got := store.Consume(g1.Token, 1, 10, now); got != nil {
		t.Error("已撤销会话的许可仍可用")
	}
	// 其他会话不受影响。
	if got := store.Consume(g2.Token, 1, 20, now); got == nil {
		t.Error("其他会话的许可被误撤销")
	}
}

func TestConsumeEmptyToken(t *testing.T) {
	store := NewStore()
	if got := store.Consume("", 1, 10, time.Now()); got != nil {
		t.Error("空令牌不应通过")
	}
}

// --- 挑战 ---

func TestChallengeSignature(t *testing.T) {
	s := newSigner([]byte("signing-key"))
	now := time.Now()

	token, err := s.Issue(7, 70, ActionVMDelete, now)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	c, err := s.Parse(token, now)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.UserID != 7 || c.SessionID != 70 || c.Action != ActionVMDelete {
		t.Errorf("解析结果不正确: %+v", c)
	}

	// 篡改任一字节都必须被发现。改签名段而非内容段：改内容会命中同一条
	// 完整性校验，改签名才能验证「签名本身被校验」。
	tampered := token[:len(token)-1] + "X"
	if _, err := s.Parse(tampered, now); err == nil {
		t.Error("被篡改的挑战通过了校验")
	}

	// 换一把密钥签发的挑战不可接受。
	other := newSigner([]byte("another-key"))
	otherToken, _ := other.Issue(7, 70, ActionVMDelete, now)
	if _, err := s.Parse(otherToken, now); err == nil {
		t.Error("异密钥签发的挑战通过了校验")
	}
}

func TestChallengeExpires(t *testing.T) {
	s := newSigner([]byte("signing-key"))
	now := time.Now()

	token, _ := s.Issue(7, 70, ActionVMDelete, now)
	if _, err := s.Parse(token, now.Add(challengeTTL+time.Second)); err == nil {
		t.Error("过期挑战仍被接受")
	}
}

func TestChallengeRejectsMalformed(t *testing.T) {
	s := newSigner([]byte("signing-key"))
	now := time.Now()

	for _, bad := range []string{"", "no-dot", ".", "a.b"} {
		if _, err := s.Parse(bad, now); err == nil {
			t.Errorf("畸形挑战 %q 通过了校验", bad)
		}
	}
}

// --- TOTP 密钥的加密存储 ---

func TestSealOpenSecretRoundTrip(t *testing.T) {
	key := testKey(t)
	const secret = "JBSWY3DPEHPK3PXP"

	sealed, err := sealSecret(key, secret)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if strings.Contains(sealed, secret) {
		t.Error("密文中出现了明文密钥")
	}

	got, err := openSecret(key, sealed)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if got != secret {
		t.Errorf("解密结果 = %q, 期望 %q", got, secret)
	}
}

func TestOpenSecretDetectsTampering(t *testing.T) {
	key := testKey(t)

	sealed, _ := sealSecret(key, "JBSWY3DPEHPK3PXP")

	// 篡改**中间**的字符，而不是最后一个。
	//
	// base64 的末字符含有填充位：改动它可能解码出完全相同的字节，
	// 那样一来「篡改」其实没有改动任何内容，测试会时灵时不灵——而且
	// 失败时的现象是「密文没被检测出篡改」，看起来像实现有漏洞。
	idx := len(sealed) / 2
	flipped := "A"
	if sealed[idx] == 'A' {
		flipped = "B"
	}
	tampered := sealed[:idx] + flipped + sealed[idx+1:]

	// 只保证保密性、不保证完整性的方案在这里是不够的——能写库的攻击者
	// 可以把密钥改成自己已知的值，从而完全绕过第二因素。AES-GCM 必须拒绝。
	if _, err := openSecret(key, tampered); err == nil {
		t.Errorf("被篡改的密文解密未报错 (原=%s 改=%s)", sealed, tampered)
	}
}

func TestOpenSecretRejectsWrongKey(t *testing.T) {
	sealed, _ := sealSecret(testKey(t), "JBSWY3DPEHPK3PXP")
	if _, err := openSecret([]byte("wrong-key"), sealed); err == nil {
		t.Error("错误密钥解密未报错")
	}
}

// --- 恢复码 ---

func TestRecoveryCodeSingleUse(t *testing.T) {
	plain, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(plain) != RecoveryCodeCount {
		t.Fatalf("恢复码数量 = %d, 期望 %d", len(plain), RecoveryCodeCount)
	}

	remaining, updated, ok := consumeRecoveryCode(hashes, plain[0])
	if !ok {
		t.Fatal("正确的恢复码被拒绝")
	}
	if remaining != RecoveryCodeCount-1 {
		t.Errorf("剩余数量 = %d, 期望 %d", remaining, RecoveryCodeCount-1)
	}

	// 用后即废（R-008）：不失效等于留下一个永久后门，一旦泄漏就长期有效。
	if _, _, ok := consumeRecoveryCode(updated, plain[0]); ok {
		t.Error("恢复码被重复使用")
	}
	// 其他码不受影响。
	if _, _, ok := consumeRecoveryCode(updated, plain[1]); !ok {
		t.Error("消费一个码影响了其他码")
	}
}

func TestRecoveryCodeNormalization(t *testing.T) {
	plain, hashes, _ := GenerateRecoveryCodes()
	target := plain[3]

	// 用户会按自己的习惯加空格或连字符分组；这些差异必须被抹平，
	// 否则会出现「明明输对了却说错误」这类极难排查的问题。
	variants := []string{
		strings.ToLower(target),
		strings.ReplaceAll(target, "-", ""),
		strings.ReplaceAll(target, "-", " "),
		" " + target + " ",
	}
	for _, v := range variants {
		if _, _, ok := consumeRecoveryCode(hashes, v); !ok {
			t.Errorf("归一化后应接受: %q", v)
		}
	}
}

func TestRecoveryCodeRejectsUnknown(t *testing.T) {
	_, hashes, _ := GenerateRecoveryCodes()
	if _, _, ok := consumeRecoveryCode(hashes, "ZZZZ-ZZZZ-ZZZZ"); ok {
		t.Error("不存在的恢复码被接受")
	}
	if _, _, ok := consumeRecoveryCode(hashes, ""); ok {
		t.Error("空恢复码被接受")
	}
}

// --- 清单与可用方式 ---

func TestPolicyCoversKnownActions(t *testing.T) {
	for _, a := range []Action{ActionVMDelete, ActionNodeRemove, ActionSessionRevoke} {
		if !Protected(a) {
			t.Errorf("%s 应在受保护清单中", a)
		}
	}
	// 查询与普通创建类不入清单：把它们列入只会让用户对验证麻木，
	// 真正危险的操作反而被忽略（Q-002）。
	for _, a := range []Action{"vm.list", "vm.create", "node.list"} {
		if Protected(a) {
			t.Errorf("%s 不应在受保护清单中", a)
		}
	}
}

func TestAvailableMethodsByBinding(t *testing.T) {
	secret := "JBSWY3DPEHPK3PXP"
	hashes := "a\nb"

	cases := []struct {
		name string
		user *model.User
		want []Method
	}{
		{"未绑定任何方式", &model.User{}, nil},
		{"仅 TOTP", &model.User{TotpEnabled: true, TotpSecretEnc: &secret}, []Method{MethodTOTP}},
		{"仅恢复码", &model.User{RecoveryCodesHash: &hashes}, []Method{MethodRecoveryCode}},
		{
			"两者都有",
			&model.User{TotpEnabled: true, TotpSecretEnc: &secret, RecoveryCodesHash: &hashes},
			[]Method{MethodTOTP, MethodRecoveryCode},
		},
		// 有密钥但未启用（绑定到一半就退出）：不能算作可用方式，
		// 否则前端会弹出一个用户其实还没配好的验证框。
		{"有密钥但未启用", &model.User{TotpSecretEnc: &secret}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AvailableMethods(tc.user)
			if len(got) != len(tc.want) {
				t.Fatalf("方式数 = %v, 期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("方式[%d] = %v, 期望 %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// --- 限流 ---

func TestAttemptLimiterBlocks(t *testing.T) {
	l := newAttemptLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < attemptLimit; i++ {
		if ok, _ := l.allow(1); !ok {
			t.Fatalf("第 %d 次尝试就被拒绝", i+1)
		}
		l.failure(1)
	}

	ok, retryAfter := l.allow(1)
	if ok {
		t.Error("超出上限后仍允许尝试")
	}
	if retryAfter <= 0 {
		t.Error("应给出可重试时间")
	}

	// 窗口过后自动恢复，不需要任何清理动作。
	now = now.Add(attemptWindow + time.Second)
	if ok, _ := l.allow(1); !ok {
		t.Error("窗口过后应恢复尝试")
	}
}

func TestAttemptLimiterSuccessResets(t *testing.T) {
	l := newAttemptLimiter()
	l.failure(1)
	l.failure(1)

	l.success(1)
	if remaining := l.remaining(1); remaining != attemptLimit {
		t.Errorf("成功后剩余次数 = %d, 期望 %d", remaining, attemptLimit)
	}
}

func TestAttemptLimiterIsPerUser(t *testing.T) {
	l := newAttemptLimiter()
	for i := 0; i < attemptLimit; i++ {
		l.failure(1)
	}

	// 一个用户被限流不应影响其他人：否则一次攻击就能让全站无法操作。
	if ok, _ := l.allow(2); !ok {
		t.Error("限流影响到了其他用户")
	}
}

func TestTOTPVerificationAgainstGeneratedSecret(t *testing.T) {
	// 用真实密钥与真实算法走一遍，确认参数（周期、位数、算法）彼此一致：
	// 任一处写错都会表现为「所有用户都无法验证」。
	secret := "JBSWY3DPEHPK3PXP"
	now := time.Now()

	user := &model.User{TotpEnabled: true}
	if r := Verify(user, secret, MethodTOTP, "000000", now); r.OK {
		t.Error("错误动态码被接受")
	}
	// 未启用的账号即使有密钥也不能通过。
	disabled := &model.User{TotpEnabled: false}
	if r := Verify(disabled, secret, MethodTOTP, "000000", now); r.OK {
		t.Error("未启用 TOTP 的账号通过了验证")
	}
	// 密钥为空（解密失败）时不能通过。
	if r := Verify(user, "", MethodTOTP, "000000", now); r.OK {
		t.Error("空密钥通过了验证")
	}
}
