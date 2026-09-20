package quotaenforce_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/quotaenforce"
)

// TestResetUsageClearsAccumulation 覆盖「重置用量」与「改上限」的区别。
//
// 只把状态改回正常而不清累计的话，下一次评估（五分钟内）会立刻重新判定
// 为超限——用户点完"重置"发现五分钟后又被限速，那比没有这个功能更让人
// 困惑。因此这里断言的是**累计行被删掉**，而不只是状态回到 ok。
func TestResetUsageClearsAccumulation(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 7, "alice")

	seedTraffic(t, db, 7, 50, 0)
	if _, err := svc.Set(ctx, quotaenforce.Request{
		NodeID: 1, UserID: 7, Dimension: model.QuotaDimTrafficIn, LimitValue: 10,
	}, admin(), "root", "10.0.0.1"); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	// 让配额进入超限状态。
	if _, err := svc.Evaluate(ctx, 1); err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	items, err := svc.List(ctx, 1)
	if err != nil {
		t.Fatalf("列出配额失败: %v", err)
	}
	if len(items) == 0 || items[0].Status != model.QuotaStatusLimited {
		t.Fatalf("配额未进入超限状态: %+v", items)
	}

	view, err := svc.ResetUsage(ctx, items[0].ID, admin(), "root", "10.0.0.1")
	if err != nil {
		t.Fatalf("重置用量失败: %v", err)
	}
	if view.Status != model.QuotaStatusOK {
		t.Errorf("重置后状态 = %q, 期望 ok", view.Status)
	}
	if view.UsedValue != 0 {
		t.Errorf("重置后用量 = %d, 期望 0（累计应被清空）", view.UsedValue)
	}

	// 累计行确实没了：只清状态的话，下面这个计数不会是 0。
	var n int64
	if err := db.Model(&model.TrafficStatDaily{}).
		Where("owner_id = ?", 7).Count(&n).Error; err != nil {
		t.Fatalf("统计累计行失败: %v", err)
	}
	if n != 0 {
		t.Errorf("累计行 = %d, 期望 0", n)
	}

	// 重置后再评估不应重新判定为超限——否则五分钟后又被限速。
	if _, err := svc.Evaluate(ctx, 1); err != nil {
		t.Fatalf("再次评估失败: %v", err)
	}
	items, err = svc.List(ctx, 1)
	if err != nil {
		t.Fatalf("列出配额失败: %v", err)
	}
	if items[0].Status != model.QuotaStatusOK {
		t.Errorf("重置后再次评估状态 = %q, 期望 ok", items[0].Status)
	}
}

// 运行时长是另一张累计表：重置流量不该顺手把时长也抹掉，那是另一个承诺。
func TestResetUsageOnlyTouchesItsOwnDimension(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedUser(t, db, 7, "alice")

	seedTraffic(t, db, 7, 5, 0)
	seedRuntimeToday(t, db, 7, 3600)

	traffic, err := svc.Set(ctx, quotaenforce.Request{
		NodeID: 1, UserID: 7, Dimension: model.QuotaDimTrafficIn, LimitValue: 10,
	}, admin(), "root", "10.0.0.1")
	if err != nil {
		t.Fatalf("设置流量配额失败: %v", err)
	}
	if _, err := svc.ResetUsage(ctx, traffic.ID, admin(), "root", "10.0.0.1"); err != nil {
		t.Fatalf("重置用量失败: %v", err)
	}

	var remaining int64
	if err := db.Model(&model.VMRuntimeDaily{}).
		Where("owner_id = ?", 7).Count(&remaining).Error; err != nil {
		t.Fatalf("统计运行时长失败: %v", err)
	}
	if remaining == 0 {
		t.Error("重置流量不应清掉运行时长累计")
	}
}

// seedRuntimeToday 写入一条今天的运行时长累计。
func seedRuntimeToday(t *testing.T, db *gorm.DB, ownerID int64, seconds int64) {
	t.Helper()
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if err := db.Create(&model.VMRuntimeDaily{
		VMID: 1, NodeID: 1, OwnerID: &ownerID, Date: day, Seconds: int(seconds),
	}).Error; err != nil {
		t.Fatalf("写入运行时长失败: %v", err)
	}
}
