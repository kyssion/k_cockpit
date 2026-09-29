package api

import (
	"context"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// newGuardServer 构造挂了防护中间件与一个探针路由的引擎。
func newGuardServer(t *testing.T, enabled func() bool) *server.Hertz {
	t.Helper()
	h := server.New()
	h.Use(SecurityHeaders(), InputFilter(enabled))
	h.GET("/api/v1/ok", func(ctx context.Context, c *app.RequestContext) {
		c.String(consts.StatusOK, "ok")
	})
	return h
}

func TestSecurityHeaders(t *testing.T) {
	h := newGuardServer(t, nil)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/ok", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200 (body=%s)", w.Code, w.Body.String())
	}
	respHeader := w.Header()
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := respHeader.Get(header); got != want {
			t.Errorf("响应头 %s = %q, 期望 %q", header, got, want)
		}
	}
}

func TestInputFilterRejects(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"空字节", "/api/v1/a%00b"},
		{"扫描器探测", "/.env"},
		{"扫描器探测带后缀", "/.git/config"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newGuardServer(t, nil)
			w := ut.PerformRequest(h.Engine, "GET", tc.path, nil)
			if w.Code != consts.StatusForbidden {
				t.Errorf("GET %s 状态码 = %d, 期望 403", tc.path, w.Code)
			}
		})
	}
}

// 穿越判定对纯函数直接测：ut 测试链路与框架解析在中间件之前就把 ".."
// 归一化掉了，端到端构造不出「中间件能看到 ../」的请求；真实流量里这类
// 请求也到不了业务路由（框架先给 404），这里是纵深防御的一层。
func TestRejectReason(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"/api/v1/../etc/passwd", "path_traversal"},
		{"/api/v1/%2e%2e%2f%2e%2e%2fetc/passwd", "path_traversal"},
		{"/api/v1/..%2fetc/passwd", "path_traversal"},
		{"/api/v1/x?a=..%2F..%2F", "path_traversal"},
		{"/api/v1/a%00b", "control_character"},
		{"/api/v1/%zz", "malformed_escape"},
		{"/api/v1/vms?name=%E8%99%9A%E6%8B%9F%E6%9C%BA", ""},
		{"/api/v1/ok", ""},
	}
	for _, tc := range cases {
		if got := rejectReason(tc.raw); got != tc.want {
			t.Errorf("rejectReason(%q) = %q, 期望 %q", tc.raw, got, tc.want)
		}
	}
}

func TestInputFilterAllowsNormalRequests(t *testing.T) {
	h := newGuardServer(t, nil)

	// 正常路径（含中文与查询参数）不应被拦。
	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/ok?q=%E8%99%9A%E6%8B%9F%E6%9C%BA", nil)
	if w.Code != consts.StatusOK {
		t.Errorf("正常请求状态码 = %d, 期望 200", w.Code)
	}
}

func TestInputFilterDisabled(t *testing.T) {
	h := newGuardServer(t, func() bool { return false })

	w := ut.PerformRequest(h.Engine, "GET", "/.env", nil)
	if w.Code == consts.StatusForbidden {
		t.Error("开关关闭时不应拦截")
	}
}
