package authkey_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/cryptoutil"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/authkey"
)

// bootstrap 是「数据库里还没有密钥时」用的初始值，长度必须 >= 32。
const bootstrap = "0123456789abcdef0123456789abcdef"

// rootA / rootB 是两把不同的根密钥，用来覆盖「换过 SESSION_SECRET」的场景。
var (
	rootA = cryptoutil.DeriveKey([]byte("root-key-a"), "k_cockpit/auth_key/v1")
	rootB = cryptoutil.DeriveKey([]byte("root-key-b"), "k_cockpit/auth_key/v1")
)

func newDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "authkey.db"),
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
	if err := db.AutoMigrate(
		&model.SessionSigningKey{}, &model.AuditLog{}, &model.SchedulerEvent{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

// activeRows 数出当前在用的密钥条数。轮换的核心不变量就是「恰好一把」：
// 0 把会让全部令牌无法校验，多于 1 把说明旧密钥没被停用（轮换没生效）。
func activeRows(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.SessionSigningKey{}).
		Where("active = ?", true).Count(&n).Error; err != nil {
		t.Fatalf("统计在用密钥失败: %v", err)
	}
	return n
}

// TestLoadSeedsOnceOnEmptyDatabase 覆盖首次启动：库里没有密钥时用配置里的
// 初始密钥建一条，并且**只建一条**——重启一次就多一条会让会话表无限增长。
func TestLoadSeedsOnceOnEmptyDatabase(t *testing.T) {
	db := newDB(t)
	svc := authkey.NewService(db, rootA, bootstrap, nil)

	kid, secret, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("首次 Load 失败: %v", err)
	}
	if string(secret) != bootstrap {
		t.Errorf("首次 Load 应返回配置里的初始密钥, 实际 %q", string(secret))
	}
	if len(kid) == 0 {
		t.Error("首次 Load 必须给出 key_id")
	}
	if n := activeRows(t, db); n != 1 {
		t.Errorf("首次 Load 后应只有 1 把在用密钥, 实际 %d", n)
	}

	// 第二次 Load 必须复用同一条，不能重建。
	kid2, secret2, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("第二次 Load 失败: %v", err)
	}
	if kid2 != kid || string(secret2) != string(secret) {
		t.Errorf("第二次 Load 换了密钥: kid %q→%q", kid, kid2)
	}
	var total int64
	if err := db.Model(&model.SessionSigningKey{}).Count(&total).Error; err != nil {
		t.Fatalf("统计密钥失败: %v", err)
	}
	if total != 1 {
		t.Errorf("重复 Load 不应新增记录, 实际共 %d 条", total)
	}
}

// TestLoadRejectsShortBootstrap 覆盖配置错误：初始密钥太短时**拒绝启动**，
// 而不是悄悄用一把弱密钥签发全部会话。
func TestLoadRejectsShortBootstrap(t *testing.T) {
	db := newDB(t)
	svc := authkey.NewService(db, rootA, "too-short", nil)

	if _, _, err := svc.Load(context.Background()); err == nil {
		t.Fatal("初始密钥不足 32 字节时必须报错")
	} else if !strings.Contains(err.Error(), "长度不足") {
		t.Errorf("报错应说明长度不足: %v", err)
	}
	if n := activeRows(t, db); n != 0 {
		t.Errorf("拒绝启动时不应写入密钥, 实际 %d 条", n)
	}
}

// TestLoadFailsWhenRootKeyChanged 覆盖「换了 SESSION_SECRET 但库还是旧的」：
// 密文解不开时必须报错，而不是返回一把空密钥去签令牌。
func TestLoadFailsWhenRootKeyChanged(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	if _, _, err := authkey.NewService(db, rootA, bootstrap, nil).Load(ctx); err != nil {
		t.Fatalf("首次 Load 失败: %v", err)
	}

	_, _, err := authkey.NewService(db, rootB, bootstrap, nil).Load(ctx)
	if err == nil {
		t.Fatal("根密钥变化后 Load 必须失败（否则会用错误密钥签名）")
	}
	if !strings.Contains(err.Error(), "无法读取签名密钥") {
		t.Errorf("报错文案应指向密钥不可读: %v", err)
	}
}

// TestRotateReplacesKeyAndKeepsOneActive 是本包最重要的一条：轮换 = 新密钥
// 生效 + 旧密钥停用 + 旧密文仍可追溯（记录不删，但不再 active）。
func TestRotateReplacesKeyAndKeepsOneActive(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := authkey.NewService(db, rootA, bootstrap, audit.NewRecorder(db))

	oldKID, oldSecret, err := svc.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	status, err := svc.Rotate(ctx, 7, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("轮换失败: %v", err)
	}
	if status.KeyID == "" || status.KeyID == oldKID {
		t.Errorf("轮换后 key_id 必须换成新的: 旧 %q 新 %q", oldKID, status.KeyID)
	}
	if status.RotatedAt == "" {
		t.Error("轮换结果必须给出 rotated_at")
	}
	if n := activeRows(t, db); n != 1 {
		t.Errorf("轮换后在用密钥应恰好 1 把, 实际 %d", n)
	}

	newKID, newSecret, err := svc.Load(ctx)
	if err != nil {
		t.Fatalf("轮换后 Load 失败: %v", err)
	}
	if newKID != status.KeyID {
		t.Errorf("Load 拿到的 kid = %q, 期望 %q", newKID, status.KeyID)
	}
	if string(newSecret) == string(oldSecret) {
		t.Error("轮换后密钥没变，旧令牌不会失效")
	}
	if len(newSecret) != 64 {
		t.Errorf("新密钥应是 32 字节的十六进制（64 字符）, 实际 %d", len(newSecret))
	}

	// 旧记录保留但停用：这是「怀疑泄漏」时唯一的追溯线索。
	var old model.SessionSigningKey
	if err := db.Where("key_id = ?", oldKID).First(&old).Error; err != nil {
		t.Fatalf("旧密钥记录不应被删除: %v", err)
	}
	if old.Active {
		t.Error("旧密钥必须被停用")
	}
	plain, err := cryptoutil.Open(rootA, old.SecretEnc)
	if err != nil {
		t.Fatalf("旧密文应仍可解密: %v", err)
	}
	if plain != string(oldSecret) {
		t.Error("旧记录里的密钥与轮换前不一致")
	}

	// 审计必须留痕：轮换让全员下线，事后一定要能回答「谁做的」。
	var entry model.AuditLog
	if err := db.Where("action = ?", "auth_key.rotate").First(&entry).Error; err != nil {
		t.Fatalf("轮换未写审计: %v", err)
	}
	if entry.OperatorID == nil || *entry.OperatorID != 7 {
		t.Errorf("审计操作人 = %v, 期望 7", entry.OperatorID)
	}
	if entry.ResourceName == nil || *entry.ResourceName != status.KeyID {
		t.Errorf("审计资源名应记录新 kid, 实际 %v", entry.ResourceName)
	}
}

// TestCurrentReportsAgeAndEmptyState 覆盖安全检查页的两个分支：
// 没有密钥时返回 nil（界面显示「尚未初始化」），有密钥时算出天数。
func TestCurrentReportsAgeAndEmptyState(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := authkey.NewService(db, rootA, bootstrap, nil)

	st, err := svc.Current(ctx)
	if err != nil {
		t.Fatalf("空库 Current 失败: %v", err)
	}
	if st != nil {
		t.Fatalf("空库 Current 应返回 nil, 实际 %+v", st)
	}

	// 直接插一条 40 天前的记录：Load 会用它，不会再走 seed。
	sealed, err := cryptoutil.Seal(rootA, bootstrap)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	rotated := time.Now().Add(-40*24*time.Hour - time.Hour)
	if err := db.Create(&model.SessionSigningKey{
		KeyID: "deadbeefdeadbeef", SecretEnc: sealed, Active: true,
		CreatedAt: rotated, RotatedAt: rotated,
	}).Error; err != nil {
		t.Fatalf("插入密钥失败: %v", err)
	}

	st, err = svc.Current(ctx)
	if err != nil {
		t.Fatalf("Current 失败: %v", err)
	}
	if st == nil {
		t.Fatal("有密钥时 Current 不应返回 nil")
	}
	if st.KeyID != "deadbeefdeadbeef" {
		t.Errorf("kid = %q", st.KeyID)
	}
	if st.AgeDays != 40 {
		t.Errorf("AgeDays = %d, 期望 40", st.AgeDays)
	}
}

// TestRotateIfDueRespectsThreshold 覆盖自动轮换的三个分支。0 表示关闭，
// 未到期什么都不做，到期才换——少了「未到期」这一条，面板会在每次 tick
// 都让全员下线。
func TestRotateIfDueRespectsThreshold(t *testing.T) {
	ctx := context.Background()

	t.Run("未开启时不动", func(t *testing.T) {
		db := newDB(t)
		svc := authkey.NewService(db, rootA, bootstrap, nil)
		kid, _, err := svc.Load(ctx)
		if err != nil {
			t.Fatalf("Load 失败: %v", err)
		}

		rotated, err := svc.RotateIfDue(ctx, 0)
		if err != nil || rotated {
			t.Fatalf("maxAgeDays=0 应什么都不做, got rotated=%v err=%v", rotated, err)
		}
		if now, _, _ := svc.Load(ctx); now != kid {
			t.Error("关闭自动轮换时密钥不应变化")
		}
	})

	t.Run("未到期不换", func(t *testing.T) {
		db := newDB(t)
		svc := authkey.NewService(db, rootA, bootstrap, nil)
		if _, _, err := svc.Load(ctx); err != nil {
			t.Fatalf("Load 失败: %v", err)
		}

		rotated, err := svc.RotateIfDue(ctx, 30)
		if err != nil || rotated {
			t.Fatalf("刚创建的密钥不应被轮换, got rotated=%v err=%v", rotated, err)
		}
	})

	t.Run("到期就换", func(t *testing.T) {
		db := newDB(t)
		sealed, err := cryptoutil.Seal(rootA, bootstrap)
		if err != nil {
			t.Fatalf("加密失败: %v", err)
		}
		old := time.Now().Add(-31 * 24 * time.Hour)
		if err := db.Create(&model.SessionSigningKey{
			KeyID: "0011223344556677", SecretEnc: sealed, Active: true,
			CreatedAt: old, RotatedAt: old,
		}).Error; err != nil {
			t.Fatalf("插入密钥失败: %v", err)
		}
		svc := authkey.NewService(db, rootA, bootstrap, nil)

		rotated, err := svc.RotateIfDue(ctx, 30)
		if err != nil {
			t.Fatalf("RotateIfDue 失败: %v", err)
		}
		if !rotated {
			t.Fatal("31 天前的密钥在 30 天阈值下应被轮换")
		}
		if kid, _, _ := svc.Load(ctx); kid == "0011223344556677" {
			t.Error("到期后 Load 仍返回旧 kid")
		}
		if n := activeRows(t, db); n != 1 {
			t.Errorf("轮换后在用密钥应恰好 1 把, 实际 %d", n)
		}
	})
}
