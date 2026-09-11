package handler_test

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

func TestHealth(t *testing.T) {
	h, _ := newTestServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/health", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusOK)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body["status"] != "ok" || body["database"] != "up" {
		t.Errorf("健康检查响应 = %v, 期望 status=ok database=up", body)
	}
}
