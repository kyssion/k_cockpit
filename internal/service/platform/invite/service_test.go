package invite_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/invite"
)

func newEnv(t *testing.T) (*gorm.DB, *invite.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "invite.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	// Create 会反查 users 表（已有账号不能再邀请），因此也要建出来。
	if err := db.AutoMigrate(&model.UserInvite{}, &model.User{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db, invite.NewService(db, nil)
}

// TestResendRejectsRevokedInvite 撤销是「收回入口」，重发不能把已撤销的
// 邀请复活。真机第 12 轮复现过：revoke 之后 resend 返回 200，status 又变回
// pending、有效期还被延长，撤销等于没做。
func TestResendRejectsRevokedInvite(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, invite.CreateRequest{
		Email: "someone@example.com", Role: "tenant",
	}, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("创建邀请失败: %v", err)
	}
	if _, err := svc.Revoke(ctx, created.ID, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("撤销邀请失败: %v", err)
	}
	var before model.UserInvite
	if err := db.First(&before, created.ID).Error; err != nil {
		t.Fatalf("读取邀请失败: %v", err)
	}

	_, err = svc.Resend(ctx, created.ID, 1, "admin", "10.0.0.1")
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict {
		t.Fatalf("已撤销邀请重发应返回 409，实际 %v", err)
	}

	var after model.UserInvite
	if err := db.First(&after, created.ID).Error; err != nil {
		t.Fatalf("重发后读取邀请失败: %v", err)
	}
	if after.RevokedAt == nil {
		t.Error("重发把 revoked_at 清掉了，撤销状态丢失")
	}
	if after.TokenHash != before.TokenHash {
		t.Error("重发换了令牌：被撤销的链接又活了一张新的")
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Error("重发延长了已撤销邀请的有效期")
	}
}

// TestCreateRejectsOversizeTTL 有效期无上限时，ttl_hours 传大会让
// time.Duration 乘法溢出，过期时间反而落到过去。
func TestCreateRejectsOversizeTTL(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()
	for i, ttl := range []int{721, 100000000} {
		_, err := svc.Create(ctx, invite.CreateRequest{
			Email: "ttl" + string(rune('a'+i)) + "@example.com",
			Role:  "tenant", TTLHours: ttl,
		}, 1, "admin", "10.0.0.1")
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
			t.Fatalf("ttl_hours=%d 应返回 400，实际 %v", ttl, err)
		}
	}
	if _, err := svc.Create(ctx, invite.CreateRequest{
		Email: "boundary@example.com", Role: "tenant", TTLHours: 720,
	}, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("720 小时边界应允许: %v", err)
	}
}
