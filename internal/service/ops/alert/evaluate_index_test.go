package alert_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/ops/alert"
)

// 本文件覆盖第 3 轮 DEF-R4-01：告警唯一索引只有 resource_id。
//
// 真机证据：`UNIQUE constraint failed: alert.resource_id (2067)` 126 次/60s、
// 5 次 SLOW SQL、SQLITE_BUSY（部署期两次应急改密因此失败）。
// 根因不是评估器逻辑，而是索引列分叉：模型只给 ResourceID 打了 tag，
// AutoMigrate 在 SQLite 上建出的是单列索引；评估器按三列查、查不到就
// Create，单列索引下必然冲突 → continue → 下一秒重试。

// TestAlertIndexIsThreeColumns 直接读数据库里的索引定义。
//
// 这条是**根因断言**：只断言"不报错"是不够的——单列索引下多数场景也能跑，
// 只有在"同 id 不同类型/种类"时才炸。索引形状本身必须钉住。
func TestAlertIndexIsThreeColumns(t *testing.T) {
	db, _ := newEnv(t)

	var sqlText string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='index' AND name=?", "uniq_alert_kind_resource").
		Scan(&sqlText).Error; err != nil {
		t.Fatalf("读索引定义失败: %v", err)
	}
	if sqlText == "" {
		t.Fatal("测试库里没有 uniq_alert_kind_resource（模型 tag 没生效）")
	}
	lower := strings.ToLower(strings.ReplaceAll(sqlText, " ", ""))
	for _, col := range []string{"kind", "resource_type", "resource_id"} {
		if !strings.Contains(lower, col) {
			t.Errorf("索引定义缺少列 %q —— 正是 DEF-R4-01 的形状: %s", col, sqlText)
		}
	}
	// 单列索引（旧形状）必须被判定为失败：它不包含另两列。
	if !strings.Contains(lower, "(kind,resource_type,resource_id)") &&
		!strings.Contains(lower, "(kind,resource_type,resource_id)") {
		// 允许列顺序带反引号/引号，做一次宽松校验：
		idx := strings.Index(lower, "(")
		if idx >= 0 && !strings.Contains(strings.NewReplacer("`", "", `"`, "").Replace(lower[idx:]), "kind,resource_type,resource_id") {
			t.Errorf("索引列不是三列有序组合: %s", sqlText)
		}
	}
}

// TestAlertSameResourceIDDifferentKind 覆盖"同一个 id、不同种类"。
//
// 这在本缺陷下**必然失败**：单列索引让第二种类插不进去，于是那一类告警
// 永远建不出来（用户永远看不到 vm_missing 之类）。
func TestAlertSameResourceIDDifferentKind(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	// 节点 1 离线 → 产生 kind=node_offline, resource_type=node, resource_id=1
	hb := time.Now().Add(-5 * time.Minute)
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOffline, LastHeartbeatAt: &hb,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("第一轮评估失败: %v", err)
	}

	// 再制造一条同 resource_id（1）但**不同 kind / resource_type** 的告警：
	// 一台 id=1 的虚拟机在虚拟化层消失。
	owner := int64(7)
	if err := db.Create(&model.VM{
		ID: 1, NodeID: 1, Name: "vm-1", OwnerID: &owner,
		Status: model.VMStatusUnknown,
	}).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	// present 带 gorm default:true，Create 时零值会被省略 → 必须显式改。
	if err := db.Model(&model.VM{}).Where("id = ?", 1).Update("present", false).Error; err != nil {
		t.Fatalf("置 present=false 失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("第二轮评估失败（单列索引下这里会一直撞唯一约束）: %v", err)
	}

	var kinds []string
	if err := db.Model(&model.Alert{}).Where("resource_id = ?", 1).
		Order("kind").Pluck("kind", &kinds).Error; err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("resource_id=1 应有 2 条不同种类的告警，实际 %d 条: %v", len(kinds), kinds)
	}
}

// TestAlertSameResourceIDDifferentResourceType 覆盖"同一个 id、不同类型"。
func TestAlertSameResourceIDDifferentResourceType(t *testing.T) {
	db, _ := newEnv(t)
	now := time.Now()

	rows := []model.Alert{
		{Kind: alert.KindNodeOffline, Level: model.AlertLevelDanger,
			ResourceType: "node", ResourceID: 1, Title: "节点离线",
			Status: model.AlertActive, FirstAt: now, LastAt: now},
		{Kind: alert.KindTaskFailed, Level: model.AlertLevelDanger,
			ResourceType: "task", ResourceID: 1, Title: "任务失败",
			Status: model.AlertActive, FirstAt: now, LastAt: now},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("插入第 %d 条失败（同 id 不同类型应可共存）: %v", i, err)
		}
	}
}

// TestEvaluateRepeatedRoundsDoNotConflict 覆盖"重复评估不砸库、last_at 前进"。
func TestEvaluateRepeatedRoundsDoNotConflict(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	hb := time.Now().Add(-5 * time.Minute)
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
		Status: model.NodeStatusOffline, LastHeartbeatAt: &hb,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	var first model.Alert
	if err := db.Where("kind = ?", alert.KindNodeOffline).First(&first).Error; err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if _, err := svc.Evaluate(ctx); err != nil {
			t.Fatalf("第 %d 次重复评估失败: %v", i+2, err)
		}
	}
	var n int64
	db.Model(&model.Alert{}).Where("kind = ?", alert.KindNodeOffline).Count(&n)
	if n != 1 {
		t.Errorf("同一问题应有且只有 1 条告警，实际 %d", n)
	}
	var again model.Alert
	if err := db.Where("kind = ?", alert.KindNodeOffline).First(&again).Error; err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	if !again.LastAt.After(first.LastAt) {
		t.Errorf("last_at 应随每轮前进：%v → %v", first.LastAt, again.LastAt)
	}
	if again.FirstAt.After(first.FirstAt) {
		t.Error("仍是同一个未恢复的问题，first_at 不该被重置")
	}
}

// TestEvaluateClearsAlertAfterResourcePurged 覆盖孤儿告警的自愈。
//
// 取舍（对应规格第 5 项）：**由评估器收尾**，不在 VM 删除路径做级联。
// 理由：告警是"此刻状态"的投影，投影跟着事实走才是单一真相；在删除路径里
// 顺手改告警表会多出一条跨域的写路径，而删除是软删+彻底删除两段，漏一段
// 就又留下孤儿。代价是最多滞后一个评估周期（5 分钟）——对"已删除的机器"
// 这种告警完全可以接受。
func TestEvaluateClearsAlertAfterResourcePurged(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	owner := int64(7)

	if err := db.Create(&model.VM{
		ID: 9, NodeID: 1, Name: "gone-vm", OwnerID: &owner,
		Status: model.VMStatusUnknown,
	}).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	if err := db.Model(&model.VM{}).Where("id = ?", 9).Update("present", false).Error; err != nil {
		t.Fatalf("置 present=false 失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	var a model.Alert
	if err := db.Where("resource_type = ? AND resource_id = ?", "vm", 9).First(&a).Error; err != nil {
		t.Fatalf("应已产生 vm_missing 告警: %v", err)
	}
	if a.Status != model.AlertActive {
		t.Fatalf("告警应为 active，实际 %q", a.Status)
	}

	// 彻底删除这台机器（记录消失）→ 下一轮应把告警收尾。
	if err := db.Unscoped().Delete(&model.VM{}, 9).Error; err != nil {
		t.Fatalf("删除虚拟机失败: %v", err)
	}
	if _, err := svc.Evaluate(ctx); err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	var after model.Alert
	if err := db.Where("id = ?", a.ID).First(&after).Error; err != nil {
		t.Fatalf("查询告警失败: %v", err)
	}
	if after.Status != model.AlertCleared {
		t.Errorf("资源已不存在时应置 cleared，实际 %q", after.Status)
	}
	if after.ClearedAt == nil {
		t.Error("应记 cleared_at")
	}
}

// TestEvaluateHandlesManyOpenAlerts 覆盖"几千条开启中的告警能跑完一轮"。
//
// 真机上曾有 4991 条孤儿告警，而收尾是**逐条 UPDATE**；这条用例确认它能在
// 一轮内收完（不是靠运气），并把耗时打出来，便于以后判断是否要批量化。
func TestEvaluateHandlesManyOpenAlerts(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	const n = 800
	now := time.Now().Add(-time.Hour)

	batch := make([]model.Alert, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, model.Alert{
			Kind: alert.KindVMMissing, Level: model.AlertLevelWarning,
			ResourceType: "vm", ResourceID: int64(100000 + i),
			Title:  "孤儿告警",
			Status: model.AlertActive, FirstAt: now, LastAt: now,
		})
	}
	if err := db.CreateInBatches(&batch, 200).Error; err != nil {
		t.Fatalf("批量写入告警失败: %v", err)
	}

	start := time.Now()
	changed, err := svc.Evaluate(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("评估失败: %v", err)
	}
	t.Logf("收尾 %d 条共 %d 条变化，耗时 %s", n, changed, elapsed)

	var still int64
	db.Model(&model.Alert{}).Where("status IN ?",
		[]string{model.AlertActive, model.AlertAcked}).Count(&still)
	if still != 0 {
		t.Errorf("一轮之后不应还有开启中的孤儿告警，实际 %d 条", still)
	}
	if elapsed > 30*time.Second {
		t.Errorf("一轮收尾耗时 %s，超出可接受范围（应批量化）", elapsed)
	}
}
