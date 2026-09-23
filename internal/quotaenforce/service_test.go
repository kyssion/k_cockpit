package quotaenforce_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/quotaenforce"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *quotaenforce.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "q.db"),
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
		&model.ResourceQuota{}, &model.TrafficStatDaily{}, &model.VMRuntimeDaily{},
		&model.User{}, &model.Node{}, &model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(quotaenforce.NewExecutor(db, client))
	return db, quotaenforce.NewService(db, q, client, recorder)
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9999, IsAdmin: true} }

func seedUser(t *testing.T, db *gorm.DB, id int64, name string) {
	t.Helper()
	if err := db.Create(&model.User{
		ID: id, Username: name, PasswordHash: "x", Status: "active",
	}).Error; err != nil {
		t.Fatalf("创建用户失败: %v", err)
	}
}

// seedTraffic **累加**今天的流量（GB → 字节）。
//
// 必须累加而不是插入：表上有 (scope_type, scope_id, date) 的唯一约束，
// 而真实的采集器也是 upsert 累加的——同一个用户、同一天、同一个作用域
// 只会有一行。测试里直接 Create 第二次会撞约束，那反映的是测试写错了，
// 不是实现对不上。
func seedTraffic(t *testing.T, db *gorm.DB, ownerID int64, gbIn, gbOut int64) {
	t.Helper()
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	row := model.TrafficStatDaily{
		ScopeType: model.TrafficScopeNode, ScopeID: 1, OwnerID: &ownerID,
		Date: day, BytesIn: gbIn * 1024 * 1024 * 1024, BytesOut: gbOut * 1024 * 1024 * 1024,
	}
	err := db.Create(&row).Error
	if err == nil {
		return
	}
	// 已存在则累加。
	if e := db.Model(&model.TrafficStatDaily{}).
		Where("scope_type = ? AND scope_id = ? AND date = ?", row.ScopeType, row.ScopeID, day).
		Updates(map[string]any{
			"bytes_in":  gorm.Expr("bytes_in + ?", row.BytesIn),
			"bytes_out": gorm.Expr("bytes_out + ?", row.BytesOut),
		}).Error; e != nil {
		t.Fatalf("累加流量失败: %v", e)
	}
}

func setQuota(t *testing.T, svc *quotaenforce.Service, userID int64, limit int64) {
	t.Helper()
	if _, err := svc.Set(context.Background(), quotaenforce.Request{
		NodeID: 1, UserID: userID, Dimension: model.QuotaDimTrafficIn,
		LimitValue: limit,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
}

// TestLimitZeroMeansUnlimited 覆盖默认不限。
//
// 默认给一个上限会让用户在自己什么都没做的时候撞上一堵看不见的墙，
// 而报错指向的是「已超出配额」，与他刚做的事无关。
func TestLimitZeroMeansUnlimited(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")
	seedTraffic(t, db, 1, 5000, 0) // 5 TB

	setQuota(t, svc, 1, 0)

	n, err := svc.Evaluate(ctx, 1)
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	if n != 0 {
		t.Errorf("上限为 0 时不该有任何状态变化，实际 %d", n)
	}

	items, _ := svc.List(ctx, 1)
	if items[0].Status != model.QuotaStatusOK {
		t.Errorf("状态 = %q, 期望 ok", items[0].Status)
	}
}

// TestWarnThenLimit 覆盖两档状态的先后与阈值。
//
// warned 与 limited **必须分开**：前者是"快到了"，后者是"已经处置了"。
// 合并成一个「超限」状态的话，用户在网络变慢时无法判断是自己用超了，
// 还是会话出了问题。
func TestWarnThenLimit(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")

	// 上限 100 GB，先用 85 GB —— 越过 80% 的预警线但未超限。
	seedTraffic(t, db, 1, 85, 0)
	setQuota(t, svc, 1, 100)

	if _, err := svc.Evaluate(ctx, 1); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	items, _ := svc.List(ctx, 1)
	if items[0].Status != model.QuotaStatusWarned {
		t.Fatalf("85/100 应为 warned，实际 %q", items[0].Status)
	}
	if items[0].UsedPercent != 85 {
		t.Errorf("用量百分比 = %d, 期望 85", items[0].UsedPercent)
	}

	// 再涨到 120 GB —— 超限。
	seedTraffic(t, db, 1, 35, 0)
	if _, err := svc.Evaluate(ctx, 1); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	items, _ = svc.List(ctx, 1)
	if items[0].Status != model.QuotaStatusLimited {
		t.Fatalf("120/100 应为 limited，实际 %q", items[0].Status)
	}
	if items[0].LimitedAt == "" {
		t.Error("处置时刻必须记下——用户问「我的网什么时候开始变慢的」，答案在这里")
	}
	if items[0].Detail == "" {
		t.Error("超限应给出说明（何时、超了多少）")
	}
}

// TestLimitedAtIsStable 覆盖处置时刻的稳定性。
//
// 每轮都覆盖的话，"什么时候被限速的"会变成"最后一次判定的时间"——而用户
// 问的恰恰是前者。
func TestLimitedAtIsStable(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")
	seedTraffic(t, db, 1, 120, 0)
	setQuota(t, svc, 1, 100)

	svc.Evaluate(ctx, 1)
	items, _ := svc.List(ctx, 1)
	first := items[0].LimitedAt

	time.Sleep(20 * time.Millisecond)
	svc.Evaluate(ctx, 1)
	items, _ = svc.List(ctx, 1)
	if items[0].LimitedAt != first {
		t.Errorf("处置时刻不该被后续判定改写: %q → %q", first, items[0].LimitedAt)
	}
}

// TestRaiseLimitClearsEnforcement 覆盖提高上限时的状态清除。
//
// 不清的话，用户明明已经合规，网络却还是慢的，而界面上显示「已超限」。
func TestRaiseLimitClearsEnforcement(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")
	seedTraffic(t, db, 1, 120, 0)
	setQuota(t, svc, 1, 100)
	svc.Evaluate(ctx, 1)

	items, _ := svc.List(ctx, 1)
	if items[0].Status != model.QuotaStatusLimited {
		t.Fatalf("前置条件不成立: %q", items[0].Status)
	}

	// 提到 500 GB。
	setQuota(t, svc, 1, 500)
	items, _ = svc.List(ctx, 1)
	if items[0].Status != model.QuotaStatusOK {
		t.Errorf("提高上限后状态 = %q, 期望 ok——否则用户已合规而网络还是慢的", items[0].Status)
	}
	if items[0].LimitedAt != "" {
		t.Error("提高上限应清掉处置时刻")
	}
}

// TestBlockIsNotDefault 覆盖默认处置方式的取舍。
//
// 断网会让业务直接中断，选它的人应当是有意为之，而不是"没注意默认值"。
func TestBlockIsNotDefault(t *testing.T) {
	db, svc := newEnv(t)
	seedUser(t, db, 1, "alice")

	view, err := svc.Set(context.Background(), quotaenforce.Request{
		NodeID: 1, UserID: 1, Dimension: model.QuotaDimTrafficIn, LimitValue: 100,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	if view.Action != model.QuotaActionThrottle {
		t.Errorf("默认处置 = %q, 期望 throttle", view.Action)
	}
}

// TestUnknownDimensionRejected 覆盖维度校验。
func TestUnknownDimensionRejected(t *testing.T) {
	db, svc := newEnv(t)
	seedUser(t, db, 1, "alice")

	_, err := svc.Set(context.Background(), quotaenforce.Request{
		NodeID: 1, UserID: 1, Dimension: "带宽", LimitValue: 10,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

func TestUnknownUserRejected(t *testing.T) {
	_, svc := newEnv(t)

	_, err := svc.Set(context.Background(), quotaenforce.Request{
		NodeID: 1, UserID: 404, Dimension: model.QuotaDimTrafficIn, LimitValue: 10,
	}, admin(), "root", "")
	assertStatus(t, err, 404)
}

// TestPeriodRolloverResets 覆盖跨月重置。
//
// 不重置的话，上个月被限速的用户在新的一月里仍然限着——而那时他的用量是 0、
// 界面上显示「已超限」，没有任何地方能解释这件事。
func TestPeriodRolloverResets(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")
	setQuota(t, svc, 1, 100)

	// 手工造一条"上个月已限速"的记录。
	old := "2020-01"
	if err := db.Model(&model.ResourceQuota{}).Where("user_id = ?", 1).
		Updates(map[string]any{
			"status": model.QuotaStatusLimited, "period": old,
			"detail": "上个月的旧记录",
		}).Error; err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
	// 直接写 limited_at（map Updates 跳过 nil，这里要的是非 nil）。
	now := time.Now()
	db.Model(&model.ResourceQuota{}).Where("user_id = ?", 1).
		Update("limited_at", now)

	// 本月用量为 0，评估一轮。
	if _, err := svc.Evaluate(ctx, 1); err != nil {
		t.Fatalf("评估失败: %v", err)
	}

	items, _ := svc.List(ctx, 1)
	if items[0].Status != model.QuotaStatusOK {
		t.Errorf("跨月后状态 = %q, 期望 ok", items[0].Status)
	}
	if items[0].LimitedAt != "" {
		t.Error("跨月应清掉处置时刻")
	}
	if items[0].Period == old {
		t.Error("周期应更新到本月")
	}
}

// TestUsageCountsOnlyCurrentPeriod 覆盖统计口径。
//
// 上个月的流量不该算进本月——那会让用户在月初就被限速，而那个数字他
// 在任何地方都对不上。
func TestUsageCountsOnlyCurrentPeriod(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")

	// 去年的 500 GB。
	oldDate := time.Date(2020, 1, 15, 0, 0, 0, 0, time.UTC)
	if err := db.Create(&model.TrafficStatDaily{
		ScopeType: model.TrafficScopeNode, ScopeID: 1, OwnerID: ptr(int64(1)),
		Date: oldDate, BytesIn: 500 * 1024 * 1024 * 1024,
	}).Error; err != nil {
		t.Fatalf("造数据失败: %v", err)
	}

	setQuota(t, svc, 1, 100)
	items, _ := svc.List(ctx, 1)
	if items[0].UsedValue != 0 {
		t.Errorf("本月用量 = %d GB, 期望 0（上个月的不该算进来）", items[0].UsedValue)
	}
	if items[0].Status != model.QuotaStatusOK {
		t.Errorf("状态 = %q, 期望 ok", items[0].Status)
	}
}

// TestDeleteRevertsEnforcement 覆盖删除配额时的撤销。
//
// 删除意味着**回到不限**，因此必须撤掉已经生效的处置——否则用户在界面上
// 看到"没有配额"，而节点上那条限速规则还挂着。
func TestDeleteRevertsEnforcement(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")
	seedTraffic(t, db, 1, 120, 0)
	setQuota(t, svc, 1, 100)
	svc.Evaluate(ctx, 1)

	items, _ := svc.List(ctx, 1)
	if err := svc.Delete(ctx, items[0].ID, admin(), "root", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// 应产生一条撤销任务。
	var undone int64
	db.Model(&model.Task{}).Where("type = ?", model.TaskQuotaEnforce).
		Count(&undone)
	if undone < 2 { // 一次限速 + 一次撤销
		t.Errorf("应产生撤销任务，实际共 %d 条处置任务", undone)
	}
}

// TestRuntimeDimension 覆盖运行时长维度。
func TestRuntimeDimension(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 1, "alice")

	now := time.Now().UTC()
	// 本月累计 120 小时。
	if err := db.Create(&model.VMRuntimeDaily{
		VMID: 1, NodeID: 1, OwnerID: ptr(int64(1)),
		Date:    time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC),
		Seconds: 120 * 3600,
	}).Error; err != nil {
		t.Fatalf("造数据失败: %v", err)
	}

	if _, err := svc.Set(ctx, quotaenforce.Request{
		NodeID: 1, UserID: 1, Dimension: model.QuotaDimRuntime, LimitValue: 100,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("设置失败: %v", err)
	}
	items, _ := svc.List(ctx, 1)
	if items[0].UsedValue != 120 {
		t.Errorf("运行时长用量 = %d 小时, 期望 120", items[0].UsedValue)
	}
	if items[0].DimUnit != "小时" {
		t.Errorf("单位 = %q", items[0].DimUnit)
	}
}

func ptr[T any](v T) *T { return &v }

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}
