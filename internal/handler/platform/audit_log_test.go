package platform_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"k_cockpit/internal/handler/platform"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/auditlog"
)

// newAuditListEngine 只挂审计列表这一条路由，中间件与生产一致。
//
// 不预置认证用户是刻意的：ViewerOf 在无用户时返回零值视角，查询会过滤到
// 不存在的 operator_id，于是返回空页——而分页回显（page / page_size）与
// 鉴权无关，正是本用例要覆盖的东西。
func newAuditListEngine(t *testing.T) *server.Hertz {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "audit.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("初始化测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	h := server.Default()
	h.Use(api.RequestID(), api.Recover())
	h.GET("/audit", platform.NewAuditLog(auditlog.NewService(db)).List)
	return h
}

// TestAuditListPageSizeHonorsContract 覆盖第 2 轮 KNOWN-001。
//
// 缺陷：handler 直接读 QueryInt，于是审计列表的分页行为与 API.md §2 的
// 「默认 20 / 最大 100」不一致——不传 page_size 返回 50 条，传 999999 返回
// 200 条，而其他所有列表都是 20 / 100。同一个契约在两张列表上给出两个答案。
//
// 断言的是**响应里回显的 page / page_size**：那才是调用方拿到的契约值，
// 而 controller 内部的钳制细节不该由调用方推断。
func TestAuditListPageSizeHonorsContract(t *testing.T) {
	h := newAuditListEngine(t)

	cases := []struct {
		name         string
		query        string
		wantPage     int
		wantPageSize int
	}{
		{"缺省 → 契约默认 20", "", 1, 20},
		{"page_size=20", "?page_size=20", 1, 20},
		{"page_size=50", "?page_size=50", 1, 50},
		{"page_size=100 恰好等于上限", "?page_size=100", 1, 100},
		{"page_size=101 → 收敛到 100", "?page_size=101", 1, 100},
		{"page_size=999999 → 收敛到 100", "?page_size=999999", 1, 100},
		{"page=0 → 1", "?page=0", 1, 20},
		{"page=-5 → 1", "?page=-5&page_size=30", 1, 30},
		{"page=3 保留", "?page=3&page_size=10", 3, 10},
		{"非法 page_size 回落默认", "?page_size=abc", 1, 20},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := ut.PerformRequest(h.Engine, "GET", "/audit"+tc.query, nil)
			if w.Code != consts.StatusOK {
				t.Fatalf("HTTP = %d，期望 200（body=%s）", w.Code, w.Body.Bytes())
			}
			var resp struct {
				Data auditlog.Page `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("解析响应失败: %v (body=%s)", err, w.Body.Bytes())
			}
			if resp.Data.Page != tc.wantPage {
				t.Errorf("page = %d，期望 %d", resp.Data.Page, tc.wantPage)
			}
			if resp.Data.PageSize != tc.wantPageSize {
				t.Errorf("page_size = %d，期望 %d（query=%q）",
					resp.Data.PageSize, tc.wantPageSize, tc.query)
			}
			// 钳制发生在契约边界（handler），服务层看到的已是合法值，因此
			// 这里不会是"被截断"。审计特有的 truncated 语义由服务层用例
			// TestOversizedPageIsTruncatedAndSaysSo 覆盖。
			if resp.Data.Truncated {
				t.Error("契约钳制不应被标记为「结果被截断」")
			}
		})
	}
}
