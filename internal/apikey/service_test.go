package apikey_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/apikey"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

func newTestEnv(t *testing.T) (*apikey.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "apikey.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.UserAPIKey{}, &model.AuthActionToken{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return apikey.NewService(db, audit.NewRecorder(db)), db
}

const userID = int64(7)

// TestPlainKeyStoredAsHashOnly 覆盖「明文只出现一次」。
//
// 库里只存哈希，服务端之后**再也拿不到**明文——因此界面必须明确告知
// 「现在就复制，关掉就没了」，而不是像别的表单那样可以随时回来再看。
func TestPlainKeyStoredAsHashOnly(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, userID, apikey.CreateRequest{}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("生成凭证失败: %v", err)
	}
	if !strings.HasPrefix(created.PlainKey, apikey.KeyPrefix) {
		t.Errorf("明文应以 %q 开头（一眼可辨的凭据）: %q", apikey.KeyPrefix, created.PlainKey)
	}
	if len(created.PlainKey) < 40 {
		t.Errorf("明文过短，熵可能不足: %d 字符", len(created.PlainKey))
	}

	var row model.UserAPIKey
	if err := db.Where("user_id = ?", userID).First(&row).Error; err != nil {
		t.Fatalf("查询记录失败: %v", err)
	}

	// 库里不能出现明文。
	if strings.Contains(row.KeyHash, created.PlainKey) || row.KeyHash == created.PlainKey {
		t.Fatal("数据库里存了明文")
	}
	// 前缀是明文的开头，用于识别。
	if !strings.HasPrefix(created.PlainKey, row.KeyPrefix) {
		t.Errorf("前缀 %q 与明文对不上", row.KeyPrefix)
	}
	// 哈希必须是 SHA-256——**不是 argon2**。理由见 model.UserAPIKey：
	// Key 是高熵随机串，而且每个请求都要校验一次，慢哈希只会带来开销。
	sum := sha256.Sum256([]byte(created.PlainKey))
	if row.KeyHash != hex.EncodeToString(sum[:]) {
		t.Error("哈希不是明文的 SHA-256")
	}

	// 查询接口不能回传明文或哈希。
	view, err := svc.Get(ctx, userID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !view.Exists || view.Prefix != row.KeyPrefix {
		t.Errorf("视图应带出前缀: %+v", view)
	}
}

// TestRotateReplacesOldKey 覆盖「一人一个」的语义。
//
// 重新生成在语义上就是**轮换**——旧的立即失效。这一点必须在界面上说清楚，
// 否则用户会以为新 Key 和旧 Key 能并存，直到某个自动化任务突然开始报 401。
func TestRotateReplacesOldKey(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	first, err := svc.Create(ctx, userID, apikey.CreateRequest{}, "alice", "")
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	second, err := svc.Create(ctx, userID, apikey.CreateRequest{}, "alice", "")
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if first.PlainKey == second.PlainKey {
		t.Fatal("轮换后明文相同——不是真的重新生成")
	}

	// 库里只有一条。
	var n int64
	db.Model(&model.UserAPIKey{}).Where("user_id = ?", userID).Count(&n)
	if n != 1 {
		t.Errorf("记录数 = %d, 期望 1（一人一个）", n)
	}

	// 旧的失效、新的可用。
	if p, _ := svc.Authenticate(ctx, first.PlainKey, ""); p != nil {
		t.Error("轮换后旧凭据仍然可用")
	}
	if p, _ := svc.Authenticate(ctx, second.PlainKey, ""); p == nil {
		t.Error("轮换后新凭据不可用")
	}
}

// TestRevokeKeepsRecord 覆盖「撤销不删记录」。
//
// 删掉它，用户看到的是「我从没生成过 Key」——与事实不符，且事后无从追查
// 「这个 Key 是什么时候被撤掉的」。
func TestRevokeKeepsRecord(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	created, _ := svc.Create(ctx, userID, apikey.CreateRequest{}, "alice", "")
	if err := svc.Revoke(ctx, userID, "alice", ""); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	var row model.UserAPIKey
	if err := db.Where("user_id = ?", userID).First(&row).Error; err != nil {
		t.Fatal("撤销后记录被删掉了——事后将无从追查")
	}
	if row.RevokedAt == nil {
		t.Error("未记录撤销时刻")
	}
	if p, _ := svc.Authenticate(ctx, created.PlainKey, ""); p != nil {
		t.Error("撤销后凭据仍然可用")
	}

	view, _ := svc.Get(ctx, userID)
	if view.Usable || view.RevokedAt == "" {
		t.Errorf("视图应显示为已撤销且不可用: %+v", view)
	}

	// 重复撤销应给出明确的「没有可撤销的」而不是静默成功。
	if err := svc.Revoke(ctx, userID, "alice", ""); err == nil {
		t.Error("重复撤销应被拒绝")
	}
}

func TestExpiredKeyIsRejected(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	created, _ := svc.Create(ctx, userID, apikey.CreateRequest{ExpiresInDays: 1}, "alice", "")
	if p, _ := svc.Authenticate(ctx, created.PlainKey, ""); p == nil {
		t.Fatal("未过期的凭据应可用")
	}

	// 把到期时间提前到过去。
	past := time.Now().Add(-time.Hour)
	if err := db.Model(&model.UserAPIKey{}).Where("user_id = ?", userID).
		Update("expires_at", past).Error; err != nil {
		t.Fatalf("改到期时间失败: %v", err)
	}
	if p, _ := svc.Authenticate(ctx, created.PlainKey, ""); p != nil {
		t.Error("过期凭据仍然可用")
	}
}

// TestAllowedIPsRestrict 覆盖「Key 泄漏后唯一还能挡住攻击者的东西」。
func TestAllowedIPsRestrict(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, userID, apikey.CreateRequest{
		AllowedIPs: []string{"203.0.113.7", "10.0.0.0/8"},
	}, "alice", "")
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}

	// 裸地址会补成 /32——留着两种写法会让比对逻辑与字符串比较对不上。
	if len(created.AllowedIPs) != 2 || created.AllowedIPs[0] != "10.0.0.0/8" {
		t.Errorf("来源列表 = %v", created.AllowedIPs)
	}

	for _, ip := range []string{"203.0.113.7", "10.1.2.3"} {
		if p, _ := svc.Authenticate(ctx, created.PlainKey, ip); p == nil {
			t.Errorf("白名单内来源 %s 应可用", ip)
		}
	}
	if p, _ := svc.Authenticate(ctx, created.PlainKey, "198.51.100.1"); p != nil {
		t.Error("白名单外的来源不应可用——这是 Key 泄漏后唯一还能挡住攻击者的东西")
	}
	// 拿不到来源时**拒绝**而不是放行：绑定 IP 的全部意义就是「只能从这里来」。
	if p, _ := svc.Authenticate(ctx, created.PlainKey, ""); p != nil {
		t.Error("来源未知时不应放行")
	}
}

func TestRejectsNonKeyShapedInput(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	// 没有固定前缀的输入一律直接拒绝，不必查库——省掉一次无谓的查询，
	// 也让「用户把会话令牌填进了 API Key 的位置」这种情况快速失败。
	for _, bad := range []string{"", "not-a-key", "Bearer abc", "kc"} {
		if p, _ := svc.Authenticate(ctx, bad, ""); p != nil {
			t.Errorf("输入 %q 不应通过", bad)
		}
	}
}

// TestFailureReasonsAreIndistinguishable 覆盖一处信息泄漏。
//
// 凭据不存在、已撤销、已过期——对外一律是同一句「凭证无效」。区分它们会
// 让攻击者能通过响应差异**枚举出有效的前缀**（「这个前缀存在但过期了」
// 已经泄漏了一条信息）；而对合法用户来说，三种情况的处理方式本来就一样：
// 重新生成一个。
func TestFailureReasonsAreIndistinguishable(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	revoked, _ := svc.Create(ctx, userID, apikey.CreateRequest{}, "alice", "")
	if err := svc.Revoke(ctx, userID, "alice", ""); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	// 已撤销与（形状合法但）不存在、格式非法，三种都返回 nil。
	for _, bad := range []string{
		revoked.PlainKey,
		apikey.KeyPrefix + strings.Repeat("0", 64),
		"garbage",
	} {
		p, err := svc.Authenticate(ctx, bad, "")
		if p != nil || err != nil {
			t.Errorf("输入 %q 应安静地返回 nil, 实际 p=%v err=%v", bad, p, err)
		}
	}
	_ = db
}

// --- 一次性令牌 ---

func TestActionTokenIsSingleUse(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	const download = "download"

	plain, _, err := svc.IssueActionToken(ctx, userID, download)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	got, err := svc.ConsumeActionToken(ctx, plain, download)
	if err != nil {
		t.Fatalf("首次消费失败: %v", err)
	}
	if got != userID {
		t.Errorf("返回的用户 = %d, 期望 %d", got, userID)
	}

	// 第二次必须失败。令牌会出现在 URL 里，也就可能出现在浏览器历史、
	// 日志、Referer 头里——允许重复使用等于把一份长期凭据泄漏到那些地方。
	if _, err := svc.ConsumeActionToken(ctx, plain, download); err == nil {
		t.Error("同一个令牌被消费了两次")
	}
}

// TestActionTokenConcurrentConsume 覆盖并发下的「只成功一次」。
//
// 消费必须在**同一个 UPDATE 语句**里完成（`used_at IS NULL` 作为条件），
// 而不是「先查、再改」——后者在并发下会让同一个令牌被换出去两次，而
// 一次性正是这个令牌存在的全部理由。
func TestActionTokenConcurrentConsume(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	const download = "download"

	plain, _, err := svc.IssueActionToken(ctx, userID, download)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	const workers = 8
	var wg sync.WaitGroup
	results := make([]error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, results[idx] = svc.ConsumeActionToken(ctx, plain, download)
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("并发消费成功 %d 次, 期望恰好 1 次", ok)
	}
}

// TestActionTokenIsPurposeBound 覆盖用途绑定。
//
// 下载令牌不该能用来做别的事。
func TestActionTokenIsPurposeBound(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	plain, _, err := svc.IssueActionToken(ctx, userID, "download")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := svc.ConsumeActionToken(ctx, plain, "delete"); err == nil {
		t.Error("下载令牌被用于另一个用途")
	}
}

func TestActionTokenExpires(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	plain, _, err := svc.IssueActionToken(ctx, userID, "download")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if err := db.Model(&model.AuthActionToken{}).
		Where("user_id = ?", userID).
		Update("expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("改到期时间失败: %v", err)
	}
	if _, err := svc.ConsumeActionToken(ctx, plain, "download"); err == nil {
		t.Error("过期令牌不应可用")
	}
}

func TestIssueTokenRequiresPurpose(t *testing.T) {
	svc, _ := newTestEnv(t)
	if _, _, err := svc.IssueActionToken(context.Background(), userID, ""); err == nil {
		t.Error("未指定用途时应拒绝——一个用途不明的令牌无法判断该不该放行")
	}
}
