package auth_test

import (
	"context"
	"testing"
	"time"

	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// TestRevokeOtherSessionsKeepsCurrent 覆盖改密码后的会话处置。
//
// 改密码最常见的动机就是"我怀疑账号被盗"。如果旧会话还活着，这个动作就
// 失去了全部意义——攻击者的会话继续有效，而用户以为已经把对方踢出去了。
//
// 同时**必须保留当前会话**：连当前那个也撤掉的话，用户改完密码立刻被登出，
// 而他会以为改密码失败了——然后重复操作，或者干脆以为系统坏了。
func TestRevokeOtherSessionsKeepsCurrent(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	user := model.User{Username: "alice", PasswordHash: "x", Status: "active"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}

	now := time.Now()
	sessions := []model.Session{
		{SessionID: "s1", UserID: user.ID, TokenType: model.TokenTypeAccess,
			IssuedAt: now, ExpiresAt: now.Add(time.Hour)},
		{SessionID: "s2", UserID: user.ID, TokenType: model.TokenTypeAccess,
			IssuedAt: now, ExpiresAt: now.Add(time.Hour)},
		{SessionID: "s3", UserID: user.ID, TokenType: model.TokenTypeAccess,
			IssuedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	for i := range sessions {
		if err := db.Create(&sessions[i]).Error; err != nil {
			t.Fatalf("创建会话失败: %v", err)
		}
	}

	// 保留 s2（当前会话）。
	kept := sessions[1]
	n, err := svc.RevokeOtherSessions(ctx, user.ID, kept.ID)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if n != 2 {
		t.Errorf("应撤销 2 个，实际 %d", n)
	}

	var alive int64
	db.Model(&model.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", user.ID).Count(&alive)
	if alive != 1 {
		t.Errorf("应只剩 1 个有效会话，实际 %d", alive)
	}

	// 剩下的必须是当前那个。
	var left model.Session
	db.Where("user_id = ? AND revoked_at IS NULL", user.ID).First(&left)
	if left.ID != kept.ID {
		t.Errorf("留下的应是当前会话（id=%d），实际 id=%d", kept.ID, left.ID)
	}
}

// TestRevokeOtherSessionsDoesNotTouchOthers 覆盖隔离。
//
// 批量撤销是最容易写错成"撤销所有人的会话"的一类操作，而那种错误在测试里
// 只表现为"别人的登录掉了"，很容易被忽略。
func TestRevokeOtherSessionsDoesNotTouchOthers(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	alice := model.User{Username: "alice", PasswordHash: "x", Status: "active"}
	bob := model.User{Username: "bob", PasswordHash: "x", Status: "active"}
	db.Create(&alice)
	db.Create(&bob)

	now := time.Now()
	bobSession := model.Session{SessionID: "bob1", UserID: bob.ID,
		TokenType: model.TokenTypeAccess, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	db.Create(&bobSession)

	aliceSession := model.Session{SessionID: "alice1", UserID: alice.ID,
		TokenType: model.TokenTypeAccess, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	db.Create(&aliceSession)

	if _, err := svc.RevokeOtherSessions(ctx, alice.ID, 0); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}

	// bob 的会话不该受影响。keepRowID=0 表示"一个都不保留"，
	// 但它仍然只作用于 alice。
	var bobAlive int64
	db.Model(&model.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", bob.ID).Count(&bobAlive)
	if bobAlive != 1 {
		t.Error("撤销 alice 的会话影响到了 bob——批量操作必须限定在 where 里")
	}
}

// TestUsernameTakenExcludesSelf 覆盖改名时的唯一性判断。
//
// 不排除自己时，用户想「把用户名改成一样的大小写」或单纯再点一次保存，
// 都会被告知"已被使用"——而那个名字正是他自己的。
func TestUsernameTakenExcludesSelf(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	alice := model.User{Username: "alice", PasswordHash: "x", Status: "active"}
	db.Create(&alice)
	db.Create(&model.User{Username: "bob", PasswordHash: "x", Status: "active"})

	// 别人的名字 → 被占用。
	taken, err := svc.UsernameTaken(ctx, "bob", alice.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !taken {
		t.Error("bob 的名字应被判为已占用")
	}

	// 自己的名字 → 不算占用。
	taken, err = svc.UsernameTaken(ctx, "alice", alice.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if taken {
		t.Error("自己的名字不该判为已占用——否则重复保存会得到一个说不通的错误")
	}

	// 没人用的名字。
	taken, _ = svc.UsernameTaken(ctx, "carol", alice.ID)
	if taken {
		t.Error("未使用的名字不该判为占用")
	}
}

// TestUpdateUserTouchesOnlyGivenFields 覆盖部分更新的语义。
//
// 整结构更新会让调用方**无意中覆盖它没有读过的字段**——典型的是一次并发
// 改名把密码哈希写回旧值，而那种问题只表现为"某个设置莫名其妙变了"。
func TestUpdateUserTouchesOnlyGivenFields(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	hash, err := auth.HashPassword("original-password")
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	user := model.User{Username: "alice", PasswordHash: hash, Status: "active", Role: "admin"}
	db.Create(&user)

	if err := svc.UpdateUser(ctx, user.ID, map[string]any{"username": "alice2"}); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	var got model.User
	db.First(&got, user.ID)
	if got.Username != "alice2" {
		t.Errorf("用户名 = %q, 期望 alice2", got.Username)
	}
	// 没被点名的字段必须原样保留。
	if got.PasswordHash != hash {
		t.Error("密码哈希被覆盖了——部分更新只应改动点名的字段")
	}
	if got.Role != "admin" {
		t.Error("角色被覆盖了")
	}
}

func TestUpdateUserNoopOnEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.UpdateUser(context.Background(), 1, nil); err != nil {
		t.Errorf("空更新不该报错: %v", err)
	}
}
