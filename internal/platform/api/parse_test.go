package api_test

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"k_cockpit/internal/platform/api"
)

// newPageEngine 构造只挂一个取分页参数的处理器，复用本包既有的 ut 范式。
func newPageEngine(capture func(page, pageSize int)) *server.Hertz {
	h := server.Default()
	h.GET("/t", func(_ context.Context, c *app.RequestContext) {
		p, ps := api.PageParams(c)
		capture(p, ps)
		api.OK(c, map[string]int{"page": p, "page_size": ps})
	})
	return h
}

// TestPageParamsClampsPageSize 覆盖 API.md §2「page_size 最大 100」的契约：
// 超上限一律钳制为 100（不返回 400），非法/非正回落到 20，合法值原样保留。
// 断言返回的 (page, pageSize) 精确值，而不是「不越界」这类恒成立弱断言。
func TestPageParamsClampsPageSize(t *testing.T) {
	cases := []struct {
		name     string
		query    string
		wantPage int
		wantSize int
	}{
		{"page_size=0 回落 20", "?page_size=0", 1, 20},
		{"page_size=-1 回落 20", "?page_size=-1", 1, 20},
		{"page_size=1 保留", "?page_size=1", 1, 1},
		{"page_size=20 默认", "?page_size=20", 1, 20},
		{"page_size=100 上限内保留", "?page_size=100", 1, 100},
		{"page_size=101 钳制为 100", "?page_size=101", 1, 100},
		{"page_size=999999 钳制为 100", "?page_size=999999", 1, 100},
		{"page_size=abc 回落 20", "?page_size=abc", 1, 20},
		{"无参数回落 (1,20)", "", 1, 20},
		{"page 合法且 page_size 超限", "?page=3&page_size=101", 3, 100},
		{"page=0 回落 1", "?page=0&page_size=50", 1, 50},
		{"page=-5 回落 1", "?page=-5&page_size=50", 1, 50},
		{"page=abc 回落 1", "?page=abc&page_size=50", 1, 50},
		{"page 与 page_size 都合法", "?page=7&page_size=25", 7, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPage, gotSize int
			h := newPageEngine(func(p, ps int) { gotPage, gotSize = p, ps })
			w := ut.PerformRequest(h.Engine, "GET", "/t"+tc.query, nil)
			if w.Code != 200 {
				t.Fatalf("HTTP = %d，期望 200（body=%s）", w.Code, w.Body.Bytes())
			}
			if gotPage != tc.wantPage || gotSize != tc.wantSize {
				t.Errorf("PageParams = (%d, %d)，期望 (%d, %d)",
					gotPage, gotSize, tc.wantPage, tc.wantSize)
			}
		})
	}
}
