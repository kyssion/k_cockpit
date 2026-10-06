package auth_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/auth"
)

// fakeActionToken 是可编程的动作令牌消费器：第一次成功、之后失效，
// 模拟「一次性」语义。
type fakeActionToken struct {
	consumed bool
	userID   int64
}

func (f *fakeActionToken) ConsumeActionToken(_ context.Context, plain, purpose string) (int64, error) {
	if plain != "tok-ok" || purpose != "download" {
		return 0, fmt.Errorf("令牌无效")
	}
	if f.consumed {
		return 0, fmt.Errorf("令牌已失效")
	}
	f.consumed = true
	return f.userID, nil
}

// 动作令牌走查询参数认证（G-53）：有效时放行、用掉后拒绝、无 Cookie 也能过。
func TestMiddlewareActionToken(t *testing.T) {
	db, svc := newAuthTestService(t)
	user := createActiveUser(t, db)

	// 先正常登录拿一个真实会话服务（构造 Middleware 需要），请求本身不带 Cookie。
	mw := auth.NewMiddleware(svc).WithActionToken(&fakeActionToken{userID: user.ID})

	h := server.New()
	h.GET("/download", mw.Require(auth.Real), func(ctx context.Context, c *app.RequestContext) {
		u := auth.CurrentUser(c)
		if u == nil {
			c.String(consts.StatusUnauthorized, "no user")
			return
		}
		c.String(consts.StatusOK, "user="+u.Username)
	})

	// 有效令牌：放行并注入用户。
	w := ut.PerformRequest(h.Engine, "GET", "/download?action_token=tok-ok", nil)
	if w.Code != consts.StatusOK || w.Body.String() != "user=active-user" {
		t.Fatalf("有效令牌应放行: code=%d body=%q", w.Code, w.Body.String())
	}

	// 同一令牌再用：已消费，拒绝。
	w = ut.PerformRequest(h.Engine, "GET", "/download?action_token=tok-ok", nil)
	if w.Code != consts.StatusUnauthorized {
		t.Fatalf("已消费的令牌应被拒绝, code=%d", w.Code)
	}

	// 非法令牌：拒绝。
	w = ut.PerformRequest(h.Engine, "GET", "/download?action_token=tok-bad", nil)
	if w.Code != consts.StatusUnauthorized {
		t.Fatalf("非法令牌应被拒绝, code=%d", w.Code)
	}
}

// newAuthTestService 构造带临时库的认证服务。
func newAuthTestService(t *testing.T) (*gorm.DB, *auth.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "auth_mw.db"),
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
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	issuer, err := auth.NewTokenIssuer(testSecret)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	return db, auth.NewService(db, issuer, audit.NewRecorder(db), auth.Config{})
}

func createActiveUser(t *testing.T, db *gorm.DB) *model.User {
	t.Helper()
	u := model.User{
		Username: "active-user", Status: model.UserStatusActive,
		Role: model.RoleTenant,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return &u
}
