package router

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"k_cockpit/internal/platform/api"
)

// 未匹配路由必须是统一错误封包，而不是 Hertz 默认的 text/plain。
//
// 实机缺陷：API-only 部署（没有前端产物时静态托管不启用）下，
// `GET /api/v1/not-exist` 返回 `text/plain "Not Found"`——客户端按 JSON
// 解析会直接抛解析错误，也拿不到错误码与 request_id。
func TestFallbackReturnsJSONNotFound(t *testing.T) {
	h := server.New()
	registerFallbacks(h)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/not-exist", nil)
	if w.Code != 404 {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（body=%s）", err, w.Body.String())
	}
	if body.Error.Code != api.CodeResourceNotFound {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, api.CodeResourceNotFound)
	}
}

// 路径存在但方法不对 → 405，而不是 404。
//
// 混成 404 会让调用方去查路径拼写，而实际只是把 GET 写成了 POST。
func TestFallbackReturnsMethodNotAllowed(t *testing.T) {
	h := server.New(server.WithHandleMethodNotAllowed(true))
	registerFallbacks(h)
	h.GET("/only-get", func(_ context.Context, c *app.RequestContext) {
		c.String(200, "ok")
	})

	w := ut.PerformRequest(h.Engine, "POST", "/only-get", nil)
	if w.Code != 405 {
		t.Fatalf("状态码 = %d，期望 405（body=%s）", w.Code, w.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body.Error.Code != api.CodeMethodNotAllowed {
		t.Errorf("错误码 = %q，期望 %q", body.Error.Code, api.CodeMethodNotAllowed)
	}
}
