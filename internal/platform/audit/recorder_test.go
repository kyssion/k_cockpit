package audit

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "audit.db"),
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
	if err := db.AutoMigrate(&model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

// TestToJSONNilAndEmpty 覆盖「不适用」与「空串」都记为 NULL。
//
// 空串与 NULL 在审计里必须区分：`params = ”` 会让「没有参数」与
// 「参数是个空字符串」看起来一样，而回看流水时正是靠这个区分
// 「调用方没传」和「调用方传了空」。
func TestToJSONNilAndEmpty(t *testing.T) {
	if got := toJSON(nil); got != nil {
		t.Errorf("nil → %v, 期望 nil", *got)
	}
	if got := toJSON(""); got != nil {
		t.Errorf(`"" → %v, 期望 nil`, *got)
	}
}

// TestToJSONPassthroughString 覆盖已序列化文本的直通。
//
// 调用方有时已经拿到了 JSON 文本（比如从事件里透传）。再 marshal 一次
// 会得到一层带引号的字符串，读起来是 `"{\"a\":1}"`。
func TestToJSONPassthroughString(t *testing.T) {
	raw := `{"a":1}`
	got := toJSON(raw)
	if got == nil || *got != raw {
		t.Errorf("字符串应原样透传，实际 %v", got)
	}
}

// TestToJSONMarshalsValue 覆盖一般值的序列化。
func TestToJSONMarshalsValue(t *testing.T) {
	got := toJSON(map[string]any{"tag": "prod"})
	if got == nil {
		t.Fatal("期望非 nil")
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(*got), &back); err != nil {
		t.Fatalf("产出不是合法 JSON: %q", *got)
	}
	if back["tag"] != "prod" {
		t.Errorf("内容 = %v", back)
	}
}

// TestToJSONPlaceholderOnUnsupported 覆盖序列化失败的兜底。
//
// 关键在「不返回 nil」：返回 nil 会让一条本该有内容的审计变成「没有参数」，
// 事后完全看不出这里曾经有过东西。占位说明让问题自己显形。
func TestToJSONPlaceholderOnUnsupported(t *testing.T) {
	got := toJSON(func() {})
	if got == nil {
		t.Fatal("序列化失败时不该返回 nil（那会把问题藏成「无参数」）")
	}
	if !strings.Contains(*got, "无法序列化") {
		t.Errorf("占位文本 = %q", *got)
	}
}

// TestOptHelpers 覆盖零值转 NULL。
func TestOptHelpers(t *testing.T) {
	if optID(0) != nil {
		t.Error("optID(0) 应为 nil（0 表示系统动作 / 不适用）")
	}
	if got := optID(42); got == nil || *got != 42 {
		t.Errorf("optID(42) = %v", got)
	}
	if optStr("") != nil {
		t.Error(`optStr("") 应为 nil`)
	}
	if got := optStr("x"); got == nil || *got != "x" {
		t.Errorf("optStr(x) = %v", got)
	}
}

// TestRecordWritesRow 覆盖一次完整写入。
func TestRecordWritesRow(t *testing.T) {
	db := newTestDB(t)
	r := NewRecorder(db)

	r.Record(context.Background(), Entry{
		OperatorID: 7, OperatorName: "alice",
		NodeID: 3, ResourceType: "vm", ResourceID: 11, ResourceName: "web-1",
		Action:   "vm.start",
		Params:   map[string]any{"force": true},
		Success:  true,
		ClientIP: "192.0.2.9",
	})

	var row model.AuditLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("读回审计失败: %v", err)
	}
	if row.Action != "vm.start" || row.ResourceType != "vm" || row.ResourceID == nil || *row.ResourceID != 11 {
		t.Errorf("记录内容 = %+v", row)
	}
	// Source 空时默认 web：否则流水里会出现一批无法归类来源的记录。
	if row.Source != model.SourceWeb {
		t.Errorf("source = %q, 期望 %q", row.Source, model.SourceWeb)
	}
	if row.Params == nil || !strings.Contains(*row.Params, "force") {
		t.Errorf("params = %v", row.Params)
	}
	// 未填的字段必须是 NULL，而不是空串——两者在筛选时行为不同。
	if row.Error != nil {
		t.Errorf("未失败的记录 error 应为 NULL，实际 %q", *row.Error)
	}
	if row.ClientIP == nil || *row.ClientIP != "192.0.2.9" {
		t.Errorf("client_ip = %v", row.ClientIP)
	}
}

// TestRecordKeepsExplicitSource 覆盖显式来源不被覆盖。
func TestRecordKeepsExplicitSource(t *testing.T) {
	db := newTestDB(t)
	NewRecorder(db).Record(context.Background(), Entry{
		Source: model.SourceScheduler, ResourceType: "node", Action: "node.heartbeat",
	})

	var row model.AuditLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if row.Source != model.SourceScheduler {
		t.Errorf("source = %q, 期望 %q", row.Source, model.SourceScheduler)
	}
}

// TestRecordFailureDoesNotPanic 覆盖「审计失败不影响主流程」。
//
// 这条约定写在包注释第一条：审计写入失败不应让业务操作失败。删表模拟
// 存储故障——Record 不返回错误，所以能验的只有「它没有 panic、也没把
// 错误抛给调用方」。失败信息由 log 承担，那部分这里不抓。
func TestRecordFailureDoesNotPanic(t *testing.T) {
	db := newTestDB(t)
	if err := db.Exec("DROP TABLE audit_log").Error; err != nil {
		t.Fatalf("删表失败: %v", err)
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("审计写入失败时不该 panic: %v", p)
		}
	}()
	NewRecorder(db).Record(context.Background(), Entry{
		ResourceType: "vm", Action: "vm.stop", Success: true,
	})
}
