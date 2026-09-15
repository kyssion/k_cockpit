package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"k_cockpit/internal/api"
)

// newEngine 构造只挂单个被测处理器的测试引擎，中间件与生产一致。
func newEngine(handler app.HandlerFunc) *server.Hertz {
	h := server.Default()
	h.Use(api.RequestID(), api.Recover())
	h.GET("/t", handler)
	return h
}

// parse 解析统一响应结构，便于各用例断言。
func parse(t *testing.T, body []byte) (map[string]any, string) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, body)
	}
	id, _ := out["request_id"].(string)
	return out, id
}

func TestOK(t *testing.T) {
	h := newEngine(func(_ context.Context, c *app.RequestContext) {
		api.OK(c, map[string]string{"name": "vm-1"})
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", w.Code)
	}

	out, id := parse(t, w.Body.Bytes())
	if id == "" {
		t.Error("响应缺少 request_id")
	}
	data, _ := out["data"].(map[string]any)
	if data["name"] != "vm-1" {
		t.Errorf("data = %v, 期望 name=vm-1", out["data"])
	}
}

func TestOKPage(t *testing.T) {
	h := newEngine(func(_ context.Context, c *app.RequestContext) {
		api.OKPage(c, []int{1, 2}, api.NewPage(2, 20, 45))
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
	out, _ := parse(t, w.Body.Bytes())

	pg, ok := out["pagination"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 pagination: %v", out)
	}
	if pg["page"] != float64(2) || pg["page_size"] != float64(20) ||
		pg["total"] != float64(45) || pg["total_pages"] != float64(3) {
		t.Errorf("pagination = %v, 期望 page=2 page_size=20 total=45 total_pages=3", pg)
	}
}

func TestNewPageRoundsUp(t *testing.T) {
	// 45 条 / 每页 20 = 3 页（向上取整，避免最后一页数据无法访问）。
	if got := api.NewPage(1, 20, 45).TotalPages; got != 3 {
		t.Errorf("TotalPages = %d, 期望 3", got)
	}
	// 整除时不应多出一页。
	if got := api.NewPage(1, 20, 40).TotalPages; got != 2 {
		t.Errorf("TotalPages = %d, 期望 2", got)
	}
	// 空结果应为 0 页。
	if got := api.NewPage(1, 20, 0).TotalPages; got != 0 {
		t.Errorf("TotalPages = %d, 期望 0", got)
	}
}

func TestFailWithBusinessError(t *testing.T) {
	cases := []struct {
		name   string
		err    *api.Error
		status int
		code   string
	}{
		{"参数不合法", api.InvalidParameter("参数校验失败"), 400, api.CodeInvalidParameter},
		{"未认证", api.Unauthenticated("请先登录"), 401, api.CodeUnauthenticated},
		{"无权限", api.PermissionDenied("无权限"), 403, api.CodePermissionDenied},
		{"资源不存在", api.NotFound("虚拟机不存在"), 404, api.CodeResourceNotFound},
		{"状态冲突", api.Conflict("当前状态不允许该操作"), 409, api.CodeResourceConflict},
		{"语义校验失败", api.ValidationFailed("磁盘容量不足"), 422, api.CodeValidationFailed},
		{"需二次验证", api.RiskVerificationRequired("该操作需要二次验证"), 428, api.CodeRiskVerification},
		{"限流", api.RateLimited("请求过于频繁"), 429, api.CodeRateLimited},
		{"依赖不可用", api.Unavailable("节点离线"), 503, api.CodeServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newEngine(func(_ context.Context, c *app.RequestContext) {
				api.Fail(c, tc.err)
			})

			w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
			if w.Code != tc.status {
				t.Fatalf("状态码 = %d, 期望 %d", w.Code, tc.status)
			}

			out, id := parse(t, w.Body.Bytes())
			if id == "" {
				t.Error("响应缺少 request_id")
			}
			errBody, ok := out["error"].(map[string]any)
			if !ok {
				t.Fatalf("响应缺少 error: %v", out)
			}
			if errBody["code"] != tc.code {
				t.Errorf("error.code = %v, 期望 %s", errBody["code"], tc.code)
			}
			if msg, _ := errBody["message"].(string); msg == "" {
				t.Error("error.message 不应为空")
			}
		})
	}
}

// 内部错误的原始信息绝不能出现在响应里——这是 API.md §3.2 的硬要求。
func TestFailInternalDoesNotLeak(t *testing.T) {
	secret := `pq: relation "vm" does not exist`

	h := newEngine(func(_ context.Context, c *app.RequestContext) {
		api.Fail(c, errors.New(secret))
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
	if w.Code != consts.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", w.Code)
	}

	out, id := parse(t, w.Body.Bytes())
	if id == "" {
		t.Error("响应缺少 request_id")
	}
	errBody, _ := out["error"].(map[string]any)
	if errBody["code"] != api.CodeInternal {
		t.Errorf("error.code = %v, 期望 %s", errBody["code"], api.CodeInternal)
	}

	raw := w.Body.String()
	for _, leak := range []string{secret, "relation", "pq:"} {
		if strings.Contains(raw, leak) {
			t.Errorf("响应泄漏了内部错误详情 %q: %s", leak, raw)
		}
	}
}

// 包装过的业务错误应被正确识别（errors.As），而不是一律退化为 500。
func TestFailUnwrapsBusinessError(t *testing.T) {
	h := newEngine(func(_ context.Context, c *app.RequestContext) {
		inner := api.Conflict("同名虚拟机已存在")
		api.Fail(c, errors.Join(errors.New("创建失败"), inner))
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
	if w.Code != consts.StatusConflict {
		t.Fatalf("状态码 = %d, 期望 409", w.Code)
	}
}

func TestRequestIDPassThrough(t *testing.T) {
	h := newEngine(func(_ context.Context, c *app.RequestContext) {
		api.OK(c, api.RequestIDFrom(c))
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil,
		ut.Header{Key: api.RequestIDHeader, Value: "trace-abc-123"})

	out, id := parse(t, w.Body.Bytes())
	if id != "trace-abc-123" {
		t.Errorf("request_id = %q, 期望透传 trace-abc-123", id)
	}
	if out["data"] != "trace-abc-123" {
		t.Errorf("处理链中的 request_id = %v, 期望 trace-abc-123", out["data"])
	}
	if got := w.Header().Get(api.RequestIDHeader); got != "trace-abc-123" {
		t.Errorf("响应头 %s = %q, 期望 trace-abc-123", api.RequestIDHeader, got)
	}
}

// 未传 request_id 时应自动生成，保证每个响应都可追溯。
func TestRequestIDGenerated(t *testing.T) {
	h := newEngine(func(_ context.Context, c *app.RequestContext) {
		api.OK(c, nil)
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
	_, id := parse(t, w.Body.Bytes())
	if len(id) != 16 {
		t.Errorf("生成的 request_id = %q (长度 %d), 期望 16 位十六进制", id, len(id))
	}
}

func TestRecoverPanic(t *testing.T) {
	h := newEngine(func(_ context.Context, _ *app.RequestContext) {
		panic("boom: internal secret")
	})

	w := ut.PerformRequest(h.Engine, "GET", "/t", nil)
	if w.Code != consts.StatusInternalServerError {
		t.Fatalf("状态码 = %d, 期望 500", w.Code)
	}
	raw := w.Body.String()
	for _, leak := range []string{"boom", "secret", "goroutine"} {
		if strings.Contains(raw, leak) {
			t.Errorf("响应泄漏了 panic 详情 %q: %s", leak, raw)
		}
	}
}
