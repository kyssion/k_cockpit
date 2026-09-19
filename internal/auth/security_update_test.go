package auth_test

import (
	"context"
	"testing"
	"time"

	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// TestSecurityUpdateInvalidatesCurrentSession 编码 R-010。
//
// f-1-01-auth-session.md R-010：
//
//	user.security_updated_at 更新（改密 / 改用户名 / 2FA 变更 / 恢复码重置）
//	→ 该用户**全部既有会话立即失效**
//
// 这道测试来自一个真实的错位。账号自管理的代码注释与界面文案都说着
// 「撤销其它会话、**保留当前这一个**」，并为此多调了一次 RevokeOtherSessions
// ——而实际上（因为同一处写了 security_updated_at）**包括当前会话在内**的
// 全部会话都会作废。
//
// 行为本身是对的（R-010 要求如此），错的是说法：用户看到「已退出其它设备上的
// N 个登录会话」，下一次点导航时自己也被登出，而界面上没有任何地方说过这件事。
//
// 因此这里断言的重点是**当前会话同样失效**——那正是当初被说反的那一点。
// 写成「只断言其它会话失效」的话，这道测试对一个「游免当前会话」的实现
// 也会通过，而那种实现恰恰会违背 R-010。
func TestSecurityUpdateInvalidatesCurrentSession(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	id := createUser(t, db, "alice", model.RoleAdmin, model.UserStatusActive)

	res, err := svc.Login(ctx, "alice", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	// 变更之前：令牌有效。
	if _, _, err := svc.Authenticate(ctx, res.Token, client, auth.Real); err != nil {
		t.Fatalf("变更之前该令牌应当有效: %v", err)
	}

	// 模拟「改密码」那一步只做的那件事：更新 security_updated_at。
	//
	// 时间要保证**晚于会话的签发时刻**，否则比对不成立——而这一点也是
	// 实现里容易踩的地方（同一秒内完成变更时，After 可能为 false）。
	time.Sleep(10 * time.Millisecond)
	if err := db.Model(&model.User{}).Where("id = ?", id).
		Update("security_updated_at", time.Now()).Error; err != nil {
		t.Fatalf("更新 security_updated_at 失败: %v", err)
	}

	if _, _, err := svc.Authenticate(ctx, res.Token, client, auth.Real); err == nil {
		t.Error("安全信息变更后，**包括当前会话在内**的全部会话都应失效（R-010）")
	}
}

// TestSecurityUpdateDoesNotAffectOtherUsers 覆盖边界。
//
// 「全部既有会话」指的是**该用户的**全部会话。按用户维度更新却影响到别人
// 的话，一次改密码会让同事掉线——那种故障在测试里只表现为「别处的登录掉了」，
// 很容易被忽略。
func TestSecurityUpdateDoesNotAffectOtherUsers(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	aliceID := createUser(t, db, "alice", model.RoleAdmin, model.UserStatusActive)
	createUser(t, db, "bob", model.RoleAdmin, model.UserStatusActive)

	bobRes, err := svc.Login(ctx, "bob", password, client)
	if err != nil {
		t.Fatalf("bob 登录失败: %v", err)
	}

	// 只动 alice。
	if err := db.Model(&model.User{}).Where("id = ?", aliceID).
		Update("security_updated_at", time.Now()).Error; err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	if _, _, err := svc.Authenticate(ctx, bobRes.Token, client, auth.Real); err != nil {
		t.Errorf("改别人的密码不该让 bob 掉线: %v", err)
	}
}
