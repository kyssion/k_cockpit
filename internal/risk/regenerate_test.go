package risk

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

func newRiskEnv(t *testing.T) (*Guard, *gorm.DB) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "risk.db"),
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
	if err := db.AutoMigrate(&model.User{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return NewGuard(db, []byte("test-root-secret-for-risk-package"), audit.NewRecorder(db), ""), db
}

// TestRegenerateInvalidatesOldCodes 覆盖「重新生成恢复码」的核心不变量。
//
// **旧码必须全部作废。** 发一批新的"万能钥匙"而旧的还能用，等于把可用的
// 入口翻了一倍，而用户以为只换了新的那一张纸——抽屉里那张旧的依然能进来，
// 而它可能已经被人抄走了。
func TestRegenerateInvalidatesOldCodes(t *testing.T) {
	guard, db := newRiskEnv(t)
	ctx := context.Background()

	oldCodes, oldHashes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("生成旧恢复码失败: %v", err)
	}
	user := model.User{
		Username: "alice", PasswordHash: "x", Status: "active",
		TotpEnabled: true, RecoveryCodesHash: &oldHashes,
	}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	newCodes, err := guard.RegenerateRecoveryCodes(ctx, &user)
	if err != nil {
		t.Fatalf("重新生成失败: %v", err)
	}
	if len(newCodes) != RecoveryCodeCount {
		t.Errorf("新恢复码数量 = %d, 期望 %d", len(newCodes), RecoveryCodeCount)
	}

	// 读回库里的哈希串——**以库为准**，而不是以返回值推断。
	var after model.User
	if err := db.First(&after, user.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if after.RecoveryCodesHash == nil {
		t.Fatal("恢复码哈希不该为空")
	}
	stored := *after.RecoveryCodesHash

	// 旧码一个都不能再通过。
	for _, c := range oldCodes {
		if _, _, ok := consumeRecoveryCode(stored, c); ok {
			t.Fatalf("旧恢复码 %q 在重新生成之后仍然有效——用户以为换掉了，实际旧的那张还能用", c)
		}
	}
	// 每个新码都必须可用。
	for _, c := range newCodes {
		if _, _, ok := consumeRecoveryCode(stored, c); !ok {
			t.Fatalf("新恢复码 %q 无法通过", c)
		}
	}
	// security_updated_at 要更新：会话层据此判定此前签发的令牌是否仍可信。
	if after.SecurityUpdatedAt == nil {
		t.Error("重新生成恢复码应更新 security_updated_at")
	}
}

// TestRegenerateRevokesPendingGrants 覆盖许可作废。
//
// 那些一次性许可是在**旧凭据**下换来的。留着它们意味着一个已经用掉恢复码
// 登进来的人，还能继续用他换到的通行证——而用户做这个动作的动机正是
// "我把手机弄丢了，想重新控制这个账号"。
func TestRegenerateRevokesPendingGrants(t *testing.T) {
	guard, db := newRiskEnv(t)
	ctx := context.Background()

	_, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	user := model.User{
		Username: "bob", PasswordHash: "x", Status: "active",
		TotpEnabled: true, RecoveryCodesHash: &hashes,
	}
	db.Create(&user)

	now := time.Now()
	grant, err := guard.grants.Issue(user.ID, 10, MethodRecoveryCode, now)
	if err != nil {
		t.Fatalf("签发许可失败: %v", err)
	}

	if _, err := guard.RegenerateRecoveryCodes(ctx, &user); err != nil {
		t.Fatalf("重新生成失败: %v", err)
	}

	if got := guard.grants.Consume(grant.Token, user.ID, 10, now); got != nil {
		t.Error("重新生成之后，旧凭据下签发的许可仍可消费")
	}
}

// TestRevokeUserGrantsScopedToUser 覆盖批量作废的隔离。
//
// 与撤销会话同理：批量操作最容易写成"清掉所有人的"。那种错误在测试里只
// 表现为"别人的操作需要重新验证"，很容易被忽略。
func TestRevokeUserGrantsScopedToUser(t *testing.T) {
	guard, _ := newRiskEnv(t)
	now := time.Now()

	aliceGrant, _ := guard.grants.Issue(1, 10, MethodTOTP, now)
	bobGrant, _ := guard.grants.Issue(2, 20, MethodTOTP, now)

	n := guard.RevokeUserGrants(1)
	if n != 1 {
		t.Errorf("应作废 1 个，实际 %d", n)
	}
	if got := guard.grants.Consume(aliceGrant.Token, 1, 10, now); got != nil {
		t.Error("alice 的许可应已作废")
	}
	if got := guard.grants.Consume(bobGrant.Token, 2, 20, now); got == nil {
		t.Error("作废 alice 的许可影响到了 bob")
	}
}
