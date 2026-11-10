package reqlog_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/reqlog"
)

func newDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "reqlog.db"),
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
	if err := db.AutoMigrate(&model.RequestLog{}, &model.User{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

func count(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.RequestLog{}).Count(&n).Error; err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	return n
}

func strPtr(s string) *string { return &s }

// TestRecordRespectsSwitch 覆盖"默认关闭"与"改了立即生效"两件事。
// 关闭时不写库是默认行为：请求日志的量级比审计大两个数量级，默认打开
// 只会把磁盘用在心跳与轮询上。
func TestRecordRespectsSwitch(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	enabled := false
	svc := reqlog.NewService(db, func(context.Context) bool { return enabled })

	if svc.Enabled(ctx) {
		t.Error("初始应为关闭")
	}
	svc.Record(ctx, reqlog.Entry{Method: "GET", Path: "/x", Status: 200})
	if n := count(t, db); n != 0 {
		t.Fatalf("关闭时不应写库, 实际 %d 条", n)
	}

	// 打开后无需重启，下一个请求就应落库。
	enabled = true
	uid := int64(9)
	svc.Record(ctx, reqlog.Entry{
		UserID: &uid, Method: "POST", Path: "/api/v1/vms", Status: 201,
		DurationMS: 42, ClientIP: strPtr("10.0.0.9"), UserAgent: strPtr("curl/8"),
	})
	if n := count(t, db); n != 1 {
		t.Fatalf("打开后应写 1 条, 实际 %d", n)
	}

	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if row.Method != "POST" || row.Path != "/api/v1/vms" || row.Status != 201 || row.DurationMS != 42 {
		t.Errorf("落库内容不符: %+v", row)
	}
	if row.UserID == nil || *row.UserID != 9 {
		t.Errorf("user_id 未记录: %v", row.UserID)
	}
	if row.ClientIP == nil || *row.ClientIP != "10.0.0.9" {
		t.Errorf("client_ip 未记录: %v", row.ClientIP)
	}
	if row.At.IsZero() {
		t.Error("at 未填写")
	}
}

// TestRecordWithoutSwitchDefaultsOff：构造时没给开关函数时一律不记录，
// 不能因为"没人告诉我"就写一堆数据。
func TestRecordWithoutSwitchDefaultsOff(t *testing.T) {
	db := newDB(t)
	svc := reqlog.NewService(db, nil)

	if svc.Enabled(context.Background()) {
		t.Error("nil 开关应视为关闭")
	}
	svc.Record(context.Background(), reqlog.Entry{Method: "GET", Path: "/x", Status: 200})
	if n := count(t, db); n != 0 {
		t.Errorf("nil 开关不应写库, 实际 %d 条", n)
	}
}

// TestListJoinsUsernameAndOrdersDesc 覆盖列表页：新人先看到最近的调用，
// 并且要显示"是谁"（只有 user_id 的列表对排查没帮助）。
func TestListJoinsUsernameAndOrdersDesc(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := reqlog.NewService(db, func(context.Context) bool { return true })

	alice := model.User{Username: "alice", PasswordHash: "x", Role: model.RoleAdmin, Status: model.UserStatusActive}
	if err := db.Create(&alice).Error; err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	base := time.Now().Add(-time.Hour)
	rows := []model.RequestLog{
		{At: base, UserID: &alice.ID, Method: "GET", Path: "/old", Status: 200},
		{At: base.Add(10 * time.Minute), Method: "GET", Path: "/anon", Status: 401},
		{At: base.Add(20 * time.Minute), UserID: &alice.ID, Method: "DELETE", Path: "/new", Status: 204},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入日志失败: %v", err)
		}
	}

	items, err := svc.List(ctx, 0, 10)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("应返回 3 条, 实际 %d", len(items))
	}
	if items[0].Path != "/new" || items[2].Path != "/old" {
		t.Errorf("排序应为 at DESC, 实际 %q ... %q", items[0].Path, items[2].Path)
	}
	if items[0].Username != "alice" {
		t.Errorf("应带出用户名, 实际 %q", items[0].Username)
	}
	if items[1].Username != "" {
		t.Errorf("匿名请求不应有用户名, 实际 %q", items[1].Username)
	}
	if items[1].Status != 401 {
		t.Errorf("状态码 = %d", items[1].Status)
	}
	if items[0].At == "" {
		t.Error("at 应格式化为字符串")
	}
}

func TestListFiltersByUser(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := reqlog.NewService(db, func(context.Context) bool { return true })

	now := time.Now()
	a, b := int64(1), int64(2)
	for _, r := range []model.RequestLog{
		{At: now, UserID: &a, Method: "GET", Path: "/a", Status: 200},
		{At: now, UserID: &b, Method: "GET", Path: "/b", Status: 200},
	} {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	items, err := svc.List(ctx, 2, 10)
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(items) != 1 || items[0].Path != "/b" {
		t.Errorf("按用户过滤结果 = %+v", items)
	}
}

// TestListClampsLimit：limit 由前端传入，越界必须被夹住，否则一次
// limit=1000000 就能把整张表拉进内存。
func TestListClampsLimit(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	svc := reqlog.NewService(db, func(context.Context) bool { return true })

	now := time.Now()
	for i := 0; i < 120; i++ {
		if err := db.Create(&model.RequestLog{
			At: now.Add(time.Duration(i) * time.Second), Method: "GET",
			Path: "/p", Status: 200,
		}).Error; err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	for _, limit := range []int{0, -5, 1000} {
		items, err := svc.List(ctx, 0, limit)
		if err != nil {
			t.Fatalf("limit=%d List 失败: %v", limit, err)
		}
		if len(items) != 100 {
			t.Errorf("limit=%d 应被夹到 100, 实际 %d 条", limit, len(items))
		}
	}
}

// TestClearAllAndByAge 覆盖两种清理诉求：清空（太占地方）与只删旧的
// （最近这些还有用）。
func TestClearAllAndByAge(t *testing.T) {
	ctx := context.Background()

	t.Run("keepDays=0 清空", func(t *testing.T) {
		db := newDB(t)
		svc := reqlog.NewService(db, func(context.Context) bool { return true })
		now := time.Now()
		for _, at := range []time.Time{now, now.AddDate(0, 0, -1), now.AddDate(0, 0, -100)} {
			if err := db.Create(&model.RequestLog{At: at, Method: "GET", Path: "/x", Status: 200}).Error; err != nil {
				t.Fatalf("插入失败: %v", err)
			}
		}

		n, err := svc.Clear(ctx, 0)
		if err != nil {
			t.Fatalf("清空失败: %v", err)
		}
		if n != 3 {
			t.Errorf("删除条数 = %d, 期望 3", n)
		}
		if left := count(t, db); left != 0 {
			t.Errorf("清空后仍有 %d 条", left)
		}
	})

	t.Run("keepDays>0 只删旧的", func(t *testing.T) {
		db := newDB(t)
		svc := reqlog.NewService(db, func(context.Context) bool { return true })
		now := time.Now()
		old := now.AddDate(0, 0, -40)
		recent := now.AddDate(0, 0, -1)
		for _, at := range []time.Time{old, recent} {
			if err := db.Create(&model.RequestLog{At: at, Method: "GET", Path: "/x", Status: 200}).Error; err != nil {
				t.Fatalf("插入失败: %v", err)
			}
		}

		n, err := svc.Clear(ctx, 30)
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if n != 1 {
			t.Errorf("删除条数 = %d, 期望 1（只删 40 天前那条）", n)
		}
		var left model.RequestLog
		if err := db.First(&left).Error; err != nil {
			t.Fatalf("应保留最近一条: %v", err)
		}
		if left.At.Before(now.AddDate(0, 0, -7)) {
			t.Error("保留的应是最近的记录")
		}
	})
}

// TestListReportsDatabaseFailure：查询失败必须报 500，而不是返回空列表——
// 空列表会被界面解释为"没有调用记录"。
func TestListReportsDatabaseFailure(t *testing.T) {
	db := newDB(t)
	svc := reqlog.NewService(db, func(context.Context) bool { return true })
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	if _, err := svc.List(context.Background(), 0, 10); err == nil {
		t.Fatal("库不可用时 List 必须报错")
	}
}

// TestRecordNeverFailsTheCaller：日志是旁路，写不进去也不能让接口失败。
func TestRecordNeverFailsTheCaller(t *testing.T) {
	db := newDB(t)
	svc := reqlog.NewService(db, func(context.Context) bool { return true })
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	svc.Record(context.Background(), reqlog.Entry{Method: "GET", Path: "/x", Status: 200})
}
