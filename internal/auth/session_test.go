package auth_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
)

const password = "correct horse battery staple"

// newTestService 构造带真实数据库（SQLite 临时文件）的认证服务。
//
// 表结构由模型自动创建：测试只用到模型已声明的字段，若代码使用了未声明
// 的列，这里会直接失败。
func newTestService(t *testing.T) (*auth.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "auth.db"),
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
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	issuer, err := auth.NewTokenIssuer(testSecret)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}

	return auth.NewService(db, issuer, audit.NewRecorder(db), auth.Config{}), db
}

// createUser 插入一个可登录的用户，返回其 ID。
func createUser(t *testing.T, db *gorm.DB, username, role, status string) int64 {
	t.Helper()

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("生成哈希失败: %v", err)
	}
	u := model.User{Username: username, PasswordHash: hash, Role: role, Status: status}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	return u.ID
}

var client = auth.ClientInfo{IP: "10.0.0.9", UserAgent: "Mozilla/5.0 (test)"}

func TestLoginSucceeds(t *testing.T) {
	svc, db := newTestService(t)
	id := createUser(t, db, "alice", model.RoleAdmin, model.UserStatusActive)

	res, err := svc.Login(context.Background(), "alice", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.User.ID != id || res.User.Username != "alice" {
		t.Errorf("返回用户 = %+v, 期望 id=%d", res.User, id)
	}
	if res.Token == "" {
		t.Error("未返回令牌")
	}
	if !res.ExpiresAt.After(time.Now()) {
		t.Error("会话过期时间应在将来")
	}

	// 会话必须落库，否则无法撤销（R-005）。
	var count int64
	db.Model(&model.Session{}).Where("user_id = ?", id).Count(&count)
	if count != 1 {
		t.Errorf("会话数 = %d, 期望 1", count)
	}
}

// R-002：登录失败必须无法区分原因，否则可枚举用户名。
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	createUser(t, db, "banned-user", model.RoleTenant, model.UserStatusBanned)
	createUser(t, db, "pending-user", model.RoleTenant, model.UserStatusPending)
	createUser(t, db, "deleted-user", model.RoleTenant, model.UserStatusActive)
	db.Model(&model.User{}).Where("username = ?", "deleted-user").Update("deleted_at", time.Now())

	cases := map[string]struct{ username, pw string }{
		"用户不存在":  {"nobody", password},
		"密码错误":   {"alice", "wrong"},
		"账号已封禁":  {"banned-user", password},
		"账号未激活":  {"pending-user", password},
		"账号已软删除": {"deleted-user", password},
	}

	var first *api.Error
	for name, tc := range cases {
		_, err := svc.Login(context.Background(), tc.username, tc.pw, client)

		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("%s: 期望业务错误, 实际 %v", name, err)
		}
		if apiErr.Status != 401 {
			t.Errorf("%s: 状态码 = %d, 期望 401", name, apiErr.Status)
		}
		if first == nil {
			first = apiErr
			continue
		}
		if apiErr.Code != first.Code || apiErr.Message != first.Message {
			t.Errorf("%s 的错误文案与其他失败不一致: %q vs %q", name, apiErr.Message, first.Message)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, err := svc.Login(context.Background(), "alice", password, client)
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	user, session, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive)
	if err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if user.Username != "alice" {
		t.Errorf("用户 = %s, 期望 alice", user.Username)
	}
	if session.UserID != user.ID {
		t.Errorf("会话归属 = %d, 期望 %d", session.UserID, user.ID)
	}
}

// R-005：签名有效但会话已撤销，必须拒绝。
func TestAuthenticateRejectsRevokedSession(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)
	if err := svc.Logout(context.Background(), sessionIDFromToken(t, res.Token), client); err != nil {
		t.Fatalf("登出失败: %v", err)
	}

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err == nil {
		t.Error("已撤销的会话仍被接受——令牌签名校验不能替代会话状态校验")
	}
}

// R-007：指纹（IP + UA）变化即拒绝，防令牌被盗用。
func TestAuthenticateRejectsFingerprintMismatch(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)

	other := auth.ClientInfo{IP: "203.0.113.7", UserAgent: client.UserAgent}
	if _, _, err := svc.Authenticate(context.Background(), res.Token, other, auth.Passive); err == nil {
		t.Error("IP 变化后仍被接受")
	}

	otherUA := auth.ClientInfo{IP: client.IP, UserAgent: "curl/8.0"}
	if _, _, err := svc.Authenticate(context.Background(), res.Token, otherUA, auth.Passive); err == nil {
		t.Error("User-Agent 变化后仍被接受")
	}
}

// R-010：安全信息变更后，全部既有会话立即失效。
func TestAuthenticateRejectsAfterSecurityUpdate(t *testing.T) {
	svc, db := newTestService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err != nil {
		t.Fatalf("改密前应可通过: %v", err)
	}

	// 模拟改密：security_updated_at 晚于会话签发时间。
	future := time.Now().Add(time.Second)
	db.Model(&model.User{}).Where("id = ?", id).Update("security_updated_at", future)

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err == nil {
		t.Error("安全信息变更后旧会话仍被接受")
	}
}

// R-011：封禁后会话立即失效。
func TestAuthenticateRejectsBannedUser(t *testing.T) {
	svc, db := newTestService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)
	db.Model(&model.User{}).Where("id = ?", id).Update("status", model.UserStatusBanned)

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err == nil {
		t.Error("被封禁用户的会话仍被接受")
	}
}

// R-011：角色每次查库读取，变更后立即生效——令牌里不含角色正是为了这一点。
func TestAuthenticateReadsRoleFromDB(t *testing.T) {
	svc, db := newTestService(t)
	id := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)

	user, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive)
	if err != nil || user.Role != model.RoleTenant {
		t.Fatalf("初始角色 = %v, err=%v", user, err)
	}

	db.Model(&model.User{}).Where("id = ?", id).Update("role", model.RoleAdmin)

	user, _, err = svc.Authenticate(context.Background(), res.Token, client, auth.Passive)
	if err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if user.Role != model.RoleAdmin {
		t.Error("角色变更未立即生效——说明角色被缓存或来自令牌")
	}
}

func TestAuthenticateRejectsBadTokens(t *testing.T) {
	svc, _ := newTestService(t)

	for _, bad := range []string{"", "garbage", "a.b.c"} {
		if _, _, err := svc.Authenticate(context.Background(), bad, client, auth.Passive); err == nil {
			t.Errorf("非法令牌 %q 被接受", bad)
		}
	}
}

// 令牌与会话必须指向同一主体：伪造的 user_id 不能通过。
func TestAuthenticateRejectsMismatchedClaims(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	otherID := createUser(t, db, "bob", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)

	// 用 alice 的会话标识、bob 的用户 ID 另签一个令牌（签名合法）。
	issuer, _ := auth.NewTokenIssuer(testSecret)
	forged, err := issuer.Issue(sessionIDFromToken(t, res.Token), otherID, model.TokenTypeAccess, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	if _, _, err := svc.Authenticate(context.Background(), forged, client, auth.Passive); err == nil {
		t.Error("令牌与会话主体不一致时仍被接受")
	}
}

func TestLogoutOnlyRevokesCurrentSession(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	first, _ := svc.Login(context.Background(), "alice", password, client)
	second, _ := svc.Login(context.Background(), "alice", password, client)

	if err := svc.Logout(context.Background(), sessionIDFromToken(t, first.Token), client); err != nil {
		t.Fatalf("登出失败: %v", err)
	}

	if _, _, err := svc.Authenticate(context.Background(), first.Token, client, auth.Passive); err == nil {
		t.Error("被登出的会话仍被接受")
	}
	if _, _, err := svc.Authenticate(context.Background(), second.Token, client, auth.Passive); err != nil {
		t.Errorf("另一会话不应受影响: %v", err)
	}
}

func TestRevokeSessionRejectsOthers(t *testing.T) {
	svc, db := newTestService(t)
	aliceID := createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)
	bobID := createUser(t, db, "bob", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "bob", password, client)
	bobSession := sessionRowIDOf(t, db, bobID)

	// alice 试图撤销 bob 的会话：必须返回 404，与「不存在」无法区分。
	err := svc.RevokeSession(context.Background(), aliceID, bobSession, client)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("越权撤销他人会话应返回 404, 实际 %v", err)
	}

	// bob 的会话仍然有效。
	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err != nil {
		t.Errorf("他人会话不应被撤销: %v", err)
	}
}

// 真实活动会延长会话；被动请求（轮询 / SSE 心跳）不会。
func TestOnlyRealActivityExtendsSession(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)
	sid := sessionIDFromToken(t, res.Token)

	// 把活跃时间改到足够早，越过写入节流窗口。
	past := time.Now().Add(-time.Hour)
	db.Model(&model.Session{}).Where("session_id = ?", sid).Update("last_active_at", past)

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if got := lastActiveOf(t, db, sid); !got.Equal(past) {
		t.Error("被动请求更新了会话活跃时间——轮询会让会话永不过期")
	}

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Real); err != nil {
		t.Fatalf("认证失败: %v", err)
	}
	if got := lastActiveOf(t, db, sid); !got.After(past) {
		t.Error("真实活动未更新会话活跃时间")
	}
}

// 会话过期后必须拒绝。
func TestAuthenticateRejectsExpiredSession(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	res, _ := svc.Login(context.Background(), "alice", password, client)
	db.Model(&model.Session{}).
		Where("session_id = ?", sessionIDFromToken(t, res.Token)).
		Update("expires_at", time.Now().Add(-time.Minute))

	if _, _, err := svc.Authenticate(context.Background(), res.Token, client, auth.Passive); err == nil {
		t.Error("过期会话仍被接受")
	}
}

// 登录成功与失败都必须留下审计（F-1-12）。
func TestLoginIsAudited(t *testing.T) {
	svc, db := newTestService(t)
	createUser(t, db, "alice", model.RoleTenant, model.UserStatusActive)

	_, _ = svc.Login(context.Background(), "alice", "wrong", client)
	_, _ = svc.Login(context.Background(), "alice", password, client)

	var logs []model.AuditLog
	db.Where("action = ?", "user.login").Order("id").Find(&logs)
	if len(logs) != 2 {
		t.Fatalf("审计记录数 = %d, 期望 2", len(logs))
	}
	if logs[0].Success {
		t.Error("失败登录被记为成功")
	}
	if !logs[1].Success {
		t.Error("成功登录被记为失败")
	}
	// 审计中不得出现凭据。
	for _, l := range logs {
		if l.Params != nil {
			t.Errorf("登录审计不应记录参数: %s", *l.Params)
		}
	}
}

// sessionIDFromToken 从令牌中取会话标识。
//
// 不能改用「查该用户最新的会话」：同一用户可能有多个并存会话（R-013 允许），
// 那样会取错对象，让「登出只影响当前会话」这类断言失去意义。
func sessionIDFromToken(t *testing.T, token string) string {
	t.Helper()
	issuer, err := auth.NewTokenIssuer(testSecret)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	claims, err := issuer.Parse(token)
	if err != nil {
		t.Fatalf("解析令牌失败: %v", err)
	}
	return claims.SessionID
}

func sessionRowIDOf(t *testing.T, db *gorm.DB, userID int64) int64 {
	t.Helper()
	var s model.Session
	if err := db.Where("user_id = ?", userID).Order("id DESC").First(&s).Error; err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}
	return s.ID
}

func lastActiveOf(t *testing.T, db *gorm.DB, sessionID string) time.Time {
	t.Helper()
	var s model.Session
	if err := db.Where("session_id = ?", sessionID).First(&s).Error; err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}
	if s.LastActiveAt == nil {
		return time.Time{}
	}
	return *s.LastActiveAt
}
