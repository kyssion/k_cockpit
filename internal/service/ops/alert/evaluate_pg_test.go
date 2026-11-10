package alert

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
)

// 本文件守住一条**只在 PostgreSQL 上暴露**的缺陷。
//
// 实机证据（r18 生产形态，PG 16）：每一轮告警评估都刷
//
//	ERROR: column reference "status" is ambiguous (SQLSTATE 42702)
//
// 原因在 `INSERT ... ON CONFLICT DO UPDATE` 的 SET 表达式：裸列名 `status`
// 在 PG 里同时能解析到目标表与 `excluded` 行，于是直接判为歧义；SQLite 不做
// 这个检查。结果面板本身不报错（评估器的错误只进日志），而告警中心永远
// 是空的——一个只在生产上「静默失效」的模块。
//
// 这里不连 PG：把子句渲染成 SQL 文本，检查 CASE 里的列**带了表名**。这条
// 断言在 sqlite 上跑得动，检出的却是 PG 的语法要求。
func TestAlertUpsertClauseQualifiesColumns(t *testing.T) {
	db, err := database.Open(config.DB{Driver: config.DriverSQLite, Path: ":memory:"}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	w := want{
		Kind: "task_failed", Level: "danger",
		ResourceType: "node", ResourceID: 1,
		ResourceName: "node-1", Title: "任务失败", Detail: "详情",
	}
	stmt := db.Session(&gorm.Session{DryRun: true}).
		Clauses(alertUpsertClause(time.Now(), w)).
		Create(&model.Alert{Kind: w.Kind, ResourceType: w.ResourceType, ResourceID: w.ResourceID}).
		Statement

	sql := stmt.SQL.String()
	if sql == "" {
		t.Fatal("DryRun 没渲染出 SQL，断言等于没跑")
	}
	if !strings.Contains(sql, "alert.status") {
		t.Errorf("CASE 里的 status 没有表名限定，PostgreSQL 会报 42702：\n%s", sql)
	}
	// 反向断言：裸的 `WHEN status` 就是缺陷形态本身。
	if strings.Contains(sql, "WHEN status ") || strings.Contains(sql, "ELSE status END") {
		t.Errorf("SQL 里仍有裸列名 status：\n%s", sql)
	}
	for _, col := range []string{"first_at", "ack_by", "ack_at", "cleared_at"} {
		if strings.Contains(sql, "WHEN status = ") {
			continue
		}
		if !strings.Contains(sql, "alert."+col) {
			t.Errorf("CASE 里的 %s 没有表名限定：\n%s", col, sql)
		}
	}
}
